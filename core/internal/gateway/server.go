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
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// Build turns configuration into a ready http.Handler.
//
// The split from Run exists so that a test can exercise the whole route table
// with httptest and no listener, and so that main stays a process wrapper.
//
// db may be nil, which is the whole-database-in-memory configuration. It is a
// parameter rather than something Build opens because the pool has a lifetime
// that outlives this function: whoever opens it closes it, and a Build that
// opened one would leak it on every error return.
//
// It also returns the refresher, because the endpoint set is live. A handler
// built over a fixed slice would be correct until the first scale, which is
// exactly the moment nobody is watching; the handler reads through a
// function instead and the refresher is what makes that function's answer
// change.
func Build(cfg Config, db *sqlstore.DB, lic entitlement.License, log *slog.Logger, version string) (http.Handler, *catalog.Refresher, error) {
	static := staticEndpoints(cfg.Upstreams)

	// Validated here as well as in Load. Load is the path a config file takes,
	// but Build is also callable with a hand-built Config, and a caller that
	// skips validation gets a gateway that serves nobody while reporting itself
	// healthy. Validating twice is free; the alternative is a check that only
	// applies to the deployments that happened to use the loader.
	if err := cfg.validate(db); err != nil {
		return nil, nil, err
	}

	keys, err := keyStore(cfg, db)
	if err != nil {
		return nil, nil, err
	}
	auth := &authn.Authenticator{Store: keys, Lic: lic}

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

	limiter, err := limiterFor(cfg, db)
	if err != nil {
		return nil, nil, err
	}
	prices, ledger := billingFor(cfg, db)

	chat := handler.NewChat(picker, proxy, tokenizer.NewResolver(0), log, handler.ChatOptions{
		MaxBytes:         int64(cfg.MaxBodyMB) << 20,
		PrefixRunes:      prefixRunes(cfg.Upstreams),
		DefaultMaxTokens: cfg.DefaultMaxTokens,
		Limiter:          limiter,
		Pricer:           prices,
		Recorder:         ledger,
	})

	current := refresher.Endpoints

	r := chi.NewRouter()
	// Order matters and is the order it is in: recoverer is outermost so a
	// panic in any later middleware still becomes an envelope; authentication
	// runs before the body is read so an invalid key costs nothing.
	r.Use(recoverer(log), requestLog(log))

	// Health is registered before the authenticated subrouter rather than
	// exempted from the middleware. A Kubernetes probe cannot hold a
	// credential, so a /healthz that returns 401 reports the pod as failing
	// while it is serving perfectly well — and the operator's response to that
	// is to remove the probe, not to fix the probe.
	r.Get("/healthz", ok)
	// Liveness for the engine side, matching what vLLM and llama.cpp serve, so
	// an operator can curl either endpoint with the same command.
	r.Get("/health", ok)

	r.Group(func(r chi.Router) {
		r.Use(authenticate(auth))

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
	})

	r.Mount("/", webui.Handler())

	return r, refresher, nil
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
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

// keyStore builds the credential store.
//
// A database, when configured, wins over the config file's key list. Not as a
// precedence rule but because the two answer different questions: the file says
// "these keys exist", the database says "these keys exist and here is what they
// are allowed to do". A gateway pointed at Postgres and holding a stale key list
// from a laptop experiment would otherwise accept credentials the tenant has
// since revoked.
//
// The return type is the interface, not a concrete pointer, and that is the
// whole point of the signature: returning a nil *authn.Memory in an interface
// field produces a non-nil interface holding a nil pointer, so `store == nil`
// downstream is false and a gateway with authentication disabled rejects every
// request because it believes it has a key store that answers "no" to
// everything. The same trap applies to a nil *postgres.KeyStore.

// Run serves until ctx is cancelled, then drains.
func Run(ctx context.Context, cfg Config, lic entitlement.License, log *slog.Logger, version string) error {
	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
	}

	handler, refresher, err := Build(cfg, db, lic, log, version)
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
