package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
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
func Build(cfg Config, lic entitlement.License, log *slog.Logger, version string) (http.Handler, error) {
	endpoints := make([]engine.Endpoint, 0, len(cfg.Upstreams))
	for _, up := range cfg.Upstreams {
		endpoints = append(endpoints, engine.Endpoint{
			ID:       up.ID,
			Model:    up.Model,
			BaseURL:  up.BaseURL,
			Replicas: up.Replicas,
			Labels:   map[string]string{"engine": up.Engine},
		})
	}

	picker := routing.NewRendezvous(endpoints)
	proxy := transport.New(transport.Options{
		Transport: outboundTransport(cfg.Timeouts),
		Authorize: upstreamAuthorizer(cfg.Upstreams),
		RetainCap: 1 << 20,
	})

	chat := handler.NewChat(picker, proxy, tokenizer.NewResolver(0), log, handler.ChatOptions{
		MaxBytes:    int64(cfg.MaxBodyMB) << 20,
		PrefixRunes: prefixRunes(cfg.Upstreams),
	})

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
		r.Get("/models", handler.NewModels(endpoints).ServeHTTP)
		r.Post("/chat/completions", chat.ServeHTTP)
	})

	r.Get("/fleet/status", (&handler.Fleet{
		Endpoints: endpoints,
		Samples:   chat.Samples,
		Lic:       lic,
		Version:   version,
	}).ServeHTTP)

	r.Mount("/", webui.Handler())

	return r, nil
}

// Run serves until ctx is cancelled, then drains.
func Run(ctx context.Context, cfg Config, lic entitlement.License, log *slog.Logger, version string) error {
	handler, err := Build(cfg, lic, log, version)
	if err != nil {
		return err
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
