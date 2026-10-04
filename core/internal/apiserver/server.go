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
	// AdminToken guards every management route. It is deliberately separate
	// from a tenant key: a tenant may spend, and this may set the price, mint
	// keys and close an invoice.
	//
	// Optional only on a loopback listen address. A server bound to the
	// wildcard without one would hand the whole money surface of the platform
	// to anything that can open a socket, so NewServer refuses that instead.
	AdminToken string
	Version    string
	Edition    string
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
	// Fail closed. A control plane bound to anything but this machine, with no
	// admin token, is not a misconfiguration to warn about at runtime — it is a
	// server where anyone who can open a socket can mint a credential, set a
	// price and close a billing period. Refusing to start is the only answer
	// that cannot be discovered after the fact.
	if cfg.AdminToken == "" && listensOffHost(cfg.Listen) {
		return nil, errs.InvalidArgument(
			"apiserver: listening on %s exposes tenants, keys and billing to the network, "+
				"so an admin token is required; set FLEET_ADMIN_TOKEN, or bind 127.0.0.1:8081",
			cfg.Listen)
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
		// Everything under the API prefix carries money or credentials, so
		// everything under it is behind the admin token — including the
		// read-only routes. A registry listing is not sensitive on its own,
		// but an unauthenticated GET that returns 200 while the mutations
		// return 401 is a map of what is worth attacking.
		r.Use(requireAdmin(s.cfg.AdminToken))

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

		tenancyRoutes(r, s)

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
		// The period still running. A separate collection rather than a query
		// on /cost-periods because it is not a period: there is no resource
		// here to close, get or delete, and it answers a question the closed
		// ones cannot.
		r.Get("/spend", s.getOpenSpend)

		// Fleet's own metering, audited against itself. Read-only, and not a
		// sub-resource of anything: it describes the ledger rather than one
		// tenant, one model or one period.
		r.Get("/usage-agreement", s.getUsageAgreement)
	})

	return r
}

// ── storage ────────────────────────────────────────────────────

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.blobs.Info(r.Context()))
}

// engineView is the console's view of one profile. It is a projection, not the
