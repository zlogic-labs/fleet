package gateway

import (
	"log/slog"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/gateway/handler"
	"github.com/zlogic-labs/fleet/core/internal/gateway/webui"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/metrics"
)

// routesDeps is everything the route table needs. Named so that adding a route
// does not mean adding a parameter to Build, which is already long enough.
type routesDeps struct {
	log       *slog.Logger
	auth      *authn.Authenticator
	lic       entitlement.License
	version   string
	registry  *metrics.Registry
	chat      *handler.Chat
	embedding *handler.Embeddings
	current   func() []engine.Endpoint
}

func routes(d routesDeps) chi.Router {
	r := chi.NewRouter()
	// Order matters and is the order it is in: recoverer is outermost so a
	// panic in any later middleware still becomes an envelope; authentication
	// runs before the body is read so an invalid key costs nothing.
	r.Use(recoverer(d.log), requestLog(d.log))

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
		r.Use(authenticate(d.auth))

		r.Route("/v1", func(r chi.Router) {
			r.Get("/models", handler.NewModels(d.current).ServeHTTP)
			r.Post("/chat/completions", d.chat.ServeHTTP)
			r.Post("/embeddings", d.embedding.ServeHTTP)
		})

		// Operational, not OpenAI-compatible, so it sits beside /v1 rather
		// than inside it: a client pointed at /v1 by an SDK must never see it,
		// and a scraper must not have to know the OpenAI prefix exists.
		r.Get("/metrics", serveMetrics(d.registry))

		r.Get("/fleet/status", (&handler.Fleet{
			Endpoints: d.current,
			Samples:   d.chat.Samples,
			Lic:       d.lic,
			Version:   d.version,
		}).ServeHTTP)
	})

	r.Mount("/", webui.Handler())

	return r
}
