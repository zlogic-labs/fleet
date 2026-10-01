package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/gateway/catalog"
	"github.com/zlogic-labs/fleet/core/internal/gateway/handler"
	"github.com/zlogic-labs/fleet/core/internal/gateway/routing"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/internal/gateway/webui"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// Build turns configuration into a ready http.Handler.
//
// The split from Run exists so that a test can exercise the whole route table
// with httptest and no listener, and so that main stays a process wrapper.
//
// It also returns the refresher, because the endpoint set is live. A handler
// built over a fixed slice would be correct until the first scale, which is
// exactly the moment nobody is watching; the handler reads through a
// function instead and the refresher is what makes that function's answer
// change.
func Build(cfg Config, lic entitlement.License, log *slog.Logger, version string) (http.Handler, *catalog.Refresher, error) {
	static := staticEndpoints(cfg.Upstreams)

	picker := routing.NewRendezvous(static)
	refresher := &catalog.Refresher{
		Client:   catalog.NewClient(cfg.ControlPlane.URL, cfg.ControlPlane.Token),
		Profiles: engine.BuiltinProfiles(),
		Log:      log,
		Interval: cfg.ControlPlane.Every,
		// Discovered endpoints are added to, never substituted for, the
		// static ones. An operator pointing at a control plane should not
		// silently lose a laptop llama.cpp declared in the config file.
		Merge: static,
		Apply: picker.Replace,
	}
	// The refresher starts with the static set, so a gateway with no control
	// plane configured behaves exactly as it did before any of this existed.
	refresher.Set(static)

	proxy := transport.New(transport.Options{
		Transport: outboundTransport(cfg.Timeouts),
		Authorize: upstreamAuthorizer(cfg.Upstreams),
		RetainCap: 1 << 20,
	})

	chat := handler.NewChat(picker, proxy, tokenizer.NewResolver(0), log, handler.ChatOptions{
		MaxBytes:    int64(cfg.MaxBodyMB) << 20,
		PrefixRunes: prefixRunes(cfg.Upstreams),
	})

	current := refresher.Endpoints

	r := chi.NewRouter()
	r.Use(recoverer(log), requestLog(log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Liveness for the engine side, matching what vLLM and llama.cpp serve, so
	// an operator can curl either endpoint with the same command.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	r.Route("/v1", func(r chi.Router) {
		r.Get("/models", handler.NewModels(current).ServeHTTP)
		r.Post("/chat/completions", chat.ServeHTTP)
	})

	r.Get("/fleet/status", (&handler.Fleet{
		Endpoints: current,
		Samples:   chat.Samples,
		Lic:       lic,
		Version:   version,
	}).ServeHTTP)

	r.Mount("/", webui.Handler())

	return r, refresher, nil
}

// staticEndpoints is the config-declared fleet, which never changes on its own.
func staticEndpoints(ups []UpstreamConfig) []engine.Endpoint {
	out := make([]engine.Endpoint, 0, len(ups))
	for _, up := range ups {
		out = append(out, engine.Endpoint{
			ID:       up.ID,
			Model:    up.Model,
			BaseURL:  up.BaseURL,
			Replicas: up.Replicas,
			Labels:   map[string]string{"engine": up.Engine},
		})
	}
	return out
}

// Run serves until ctx is cancelled, then drains.
func Run(ctx context.Context, cfg Config, lic entitlement.License, log *slog.Logger, version string) error {
	handler, refresher, err := Build(cfg, lic, log, version)
	if err != nil {
		return err
	}

	if cfg.ControlPlane.URL != "" {
		// The picker the chat handler holds is the one the refresher pushes
		// into, so a discovered deployment is routable on the next request
		// rather than the next restart.
		go func() {
			if err := refresher.Run(ctx); err != nil {
				log.Error("endpoint refresh stopped", "err", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// No WriteTimeout: a streamed completion can legitimately run for
		// minutes, and a write deadline would cut it off mid-answer. The
		// per-request context is what bounds a client that has gone away.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Every in-flight request derives from ctx, so a signal cancels
		// generation rather than leaving streams hanging until the deadline.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("fleet-gateway listening",
		"addr", cfg.Listen, "edition", lic.Edition, "version", version,
		"upstreams", len(cfg.Upstreams), "ui", "http://"+cfg.Listen+"/")

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// A bounded drain: in-flight streams need time to finish writing, but a
	// wedged one must not stop the pod from exiting.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("fleet-gateway stopped")
	return nil
}

func prefixRunes(ups []UpstreamConfig) int {
	for _, up := range ups {
		if up.AffinityPrefixRunes > 0 {
			return up.AffinityPrefixRunes
		}
	}
	return 512
}
