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
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
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
	// DB is where tenants, keys, budgets and the ledger live.
	DB *sqlstore.DB
	// Policies, Keys and Quota are the stores the tenancy routes write
	// through. Required whenever DB is set.
	Policies *sqlstore.PolicySource
	Keys     *sqlstore.KeyStore
	Quota    *sqlstore.Quota
	Cost     *sqlstore.CostStore
	// PullConcurrency is how many pulls may run at once.
	PullConcurrency int
	// FileConcurrency is how many files within one pull may download at once.
	FileConcurrency int
	// AllowedOrigins is the CORS allowlist. Empty means loopback only.
	AllowedOrigins []string
	Version        string
	Edition        string
}

const apiPrefix = "/api/v1"

// mustRouteTheContract fails at startup, not silently in production, if the
// route the control plane serves and the constant the operator PUTs to ever
// disagree. The two live in different files because chi splits the path, and
// a version bump that moved one and not the other would send every cluster's
// inventory into a void that answers 404.
func mustRouteTheContract() {
	if apiPrefix+"/inventory" != inventory.Path {
		panic("apiserver: the inventory route and inventory.Path disagree")
	}
}

func init() { mustRouteTheContract() }

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
	db       *sqlstore.DB
	policies *sqlstore.PolicySource
	keys     *sqlstore.KeyStore
	quota    *sqlstore.Quota
	cost     *sqlstore.CostStore
}

func NewServer(cfg Config, store registry.Store, log *slog.Logger) (*Server, error) {
	if store == nil {
		store = registry.NewMemory()
	}
	// The database is optional. Without one the gateway, the console and the
	// pull queue all work; only tenancy is missing, and its routes say so
	// rather than answering with an empty list that reads as "no tenants".
	// A laptop with no PostgreSQL is a supported way to try Fleet.
	if cfg.DB != nil && (cfg.Policies == nil || cfg.Keys == nil || cfg.Quota == nil || cfg.Cost == nil) {
		return nil, errs.InvalidArgument("apiserver: Policies, Keys, Quota and Cost are required with DB")
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
		db: cfg.DB, policies: cfg.Policies, keys: cfg.Keys, quota: cfg.Quota, cost: cfg.Cost,
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

	r.Route(apiPrefix, func(r chi.Router) {
		// A wildcard, not a named parameter. A model name is owner/name, and
		// chi's {name} and {name:.+} both stop at the first slash, so
		// GET /models/Qwen/Qwen2.5 would 404 while
		// GET /models/Qwen%2FQwen2.5 would work. "*" takes the whole tail and
		// modelName unescapes it, so both forms land on the same entry.
		r.Get("/models", s.listModels)
		r.Get("/models/*", s.getModel)
		r.Delete("/models/*", s.deleteModel)
		// A repository is what is stored under a name: the weight set, whether
		// or not an engine can load it. Asking whether it is usable is a
		// property of that resource, so it is a GET returning the resource, not
		// a verb-named route that performs an action.
		//
		// It cannot live at /models/*/usability: chi will not match a fixed
		// segment after a wildcard, and /models/owner/usable would be
		// ambiguous with a model named "usable".
		r.Get("/repositories/*", s.getRepository)

		r.Post("/pulls", s.startPull)
		r.Get("/pulls", s.listPulls)
		r.Get("/pulls/{id}", s.getPull)
		r.Delete("/pulls/{id}", s.cancelPull)

		r.Get("/storage", s.storageInfo)

		// The engine catalogue, so the console can show which engines can load
		// which model instead of offering a combination refused at admission.
		r.Get("/engines", s.listEngines)

		// The operator replaces what it observes. PUT rather than POST because
		// the payload is the whole cluster state and posting it twice must
		// leave the same state, not two copies.
		//
		// The path is split across this prefix and the leaf, while the operator
		// PUTs to the single constant inventory.Path. See mustRouteTheContract.
		r.Put("/inventory", s.reportInventory)
		r.Get("/clusters", s.clusterStatus)
		r.Get("/deployments", s.listDeployments)

		// Tenancy. Every collection is top-level and plural; an item's id may
		// contain a slash, so item routes end in "*" rather than "{id}" —
		// chi's named parameters stop at the first slash, which would make
		// acme/research unreachable while acme%2Fresearch worked.
		r.Get("/tenants", s.listTenants)
		r.Post("/tenants", s.createTenant)
		r.Get("/tenants/*", s.getTenant)
		r.Patch("/tenants/*", s.updateTenant)
		r.Delete("/tenants/*", s.deleteTenant)

		r.Get("/projects", s.listProjects)
		r.Post("/projects", s.createProject)
		r.Get("/projects/*", s.getProject)
		r.Patch("/projects/*", s.updateProject)
		r.Delete("/projects/*", s.deleteProject)

		r.Get("/keys", s.listKeys)
		r.Post("/keys", s.createKey)
		r.Delete("/keys/*", s.deleteKey)

		r.Get("/budget-rules", s.listBudgetRules)
		r.Post("/budget-rules", s.putBudgetRule)
		r.Delete("/budget-rules/*", s.deleteBudgetRule)

		// The cost pool. Rates are what a GPU-hour costs the operator, which
		// Fleet cannot know; periods are closed reports and are immutable.
		r.Get("/cost-rates", s.listCostRates)
		r.Post("/cost-rates", s.putCostRate)
		r.Get("/cost-periods", s.listCostPeriods)
		// PUT, not POST: closing creates the period resource at its own URL.
		// A second close is a 409 rather than a silent replacement, because an
		// invoice that can change after it was sent is not an invoice.
		r.Put("/cost-periods/{period}", s.closeCostPeriod)
		r.Get("/cost-periods/{period}", s.getCostPeriod)
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
