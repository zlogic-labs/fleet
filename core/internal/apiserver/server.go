// Package apiserver is the control plane: the management API behind the
// console's Models and Cluster pages.
//
// It is a separate process from the gateway and a separate Go module concern,
// because it has a different failure domain. The gateway must stay up when a
// 140 GiB pull is saturating a link; the control plane can be busy, restarting
// and rebuilding for an hour without a single request being affected.
package apiserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Config is the control plane's configuration.
type Config struct {
	Listen string
	// Blobs is the object store. Nil in dev, where a directory is used.
	Blobs blobstore.Store
	// Hub is the model repository. Nil means the stub, which only has
	// synthetic repositories.
	Hub hub.Hub
	// Token authenticates to gated Hub repositories.
	Token string
	// PullConcurrency is how many pulls may run at once.
	PullConcurrency int
	// FileConcurrency is how many files within one pull may download at once.
	FileConcurrency int
	// AllowedOrigins is the CORS allowlist. Empty means loopback only.
	AllowedOrigins []string
	Version        string
	Edition        string
}

// Server owns the control plane's dependencies.
type Server struct {
	cfg      Config
	store    registry.Store
	blobs    blobstore.Store
	hub      hub.Hub
	profiles *engine.Profiles
	puller   *Puller
	worker   *registry.Worker
	log      *slog.Logger
}

func NewServer(cfg Config, store registry.Store, log *slog.Logger) (*Server, error) {
	if store == nil {
		store = registry.NewMemory()
	}
	blobs := cfg.Blobs
	if blobs == nil {
		return nil, errs.Internal(errNoStore)
	}
	if err := blobs.EnsureBucket(context.Background()); err != nil {
		return nil, errs.Internal(err)
	}
	h := cfg.Hub
	if h == nil {
		h = hub.NewStub()
	}
	profiles := engine.BuiltinProfiles()

	puller := NewPuller(store, blobs, h, profiles, log)
	puller.FileConcurrency = cfg.FileConcurrency
	worker := registry.NewWorker(store, cfg.PullConcurrency)
	worker.Start(puller.Run)

	return &Server{
		cfg: cfg, store: store, blobs: blobs, hub: h, profiles: profiles,
		puller: puller, worker: worker, log: log,
	}, nil
}

func (s *Server) Close() {
	// Drains the queue and lets running pulls finish; a 40 GiB pull should not
	// be abandoned because the control plane is restarting.
	s.worker.Close()
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(cors(s.cfg.AllowedOrigins), requestLog(s.log), recoverer(s.log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	r.Get("/readyz", s.ready)

	r.Route("/api/v1", func(r chi.Router) {
		// A wildcard, not a named parameter. A model name is owner/name, and
		// chi's {name} and {name:.+} both stop at the first slash, so
		// GET /models/Qwen/Qwen2.5 would 404 while
		// GET /models/Qwen%2FQwen2.5 would work. "*" takes the whole tail and
		// modelName unescapes it, so both forms land on the same entry.
		r.Get("/models", s.listModels)
		r.Get("/models/*", s.getModel)
		r.Delete("/models/*", s.deleteModel)
		// Verify lives under its own prefix rather than as /models/*/verify:
		// a wildcard cannot be followed by a fixed segment, and
		// /models/owner/verify would be ambiguous with a model named verify.
		// GET is the real method — the check only lists objects — and the
		// engine it targets arrives as a query parameter, which a link and a
		// curl can carry and a body cannot.
		r.Get("/verify/*", s.verifyModel)
		r.Post("/verify/*", s.verifyModel)

		r.Post("/pulls", s.startPull)
		r.Get("/pulls", s.listPulls)
		r.Get("/pulls/{id}", s.getPull)
		r.Delete("/pulls/{id}", s.cancelPull)

		r.Get("/storage", s.storageInfo)

		// The engine catalogue. It is data, and exposing it lets the console
		// show which engines can load which model instead of offering a
		// combination that will be refused at admission.
		r.Get("/engines", s.listEngines)

		// The operator reports here. Neither this nor the read side knows
		// what Kubernetes is; the operator is the only component that does.
		r.Post("/operator/inventory", s.reportInventory)
		r.Get("/cluster", s.clusterStatus)
		r.Get("/deployments", s.listDeployments)
	})

	return r
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	// Readiness fails when storage is unreachable. A control plane that cannot
	// reach its weights cannot start a pull, and a deployment it reports as
	// healthy will not be able to mount anything.
	info := s.blobs.Info(r.Context())
	if !info.Reachable {
		openai.WriteError(w, errs.Unavailable("object storage is not reachable: %s", info.Message))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ── storage ────────────────────────────────────────────────────

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.blobs.Info(r.Context()))
}

// engineView is the console's view of one profile. It is a projection, not the
