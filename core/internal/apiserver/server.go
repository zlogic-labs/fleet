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

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Config is the control plane's configuration.
type Config struct {
	Listen string
	// Blobs is the object store. Nil in dev, where a directory is used.
	Blobs blobstore.Store
	// Hub is the model repository. Nil means the stub, which only has
	// synthetic repositories.
	Hub hub.Hub
	// DB is where tenants, keys, budgets and the ledger live.
	DB *sqlstore.DB
	// Policies, Keys and Quota are the stores the tenancy routes write
	// through. Required whenever DB is set.
	Policies *sqlstore.PolicySource
	Keys     *sqlstore.KeyStore
	Quota    *sqlstore.Quota
	Cost     *sqlstore.CostStore
	// Prices is where token price books are declared and read back. Separate
	// from Cost because it is a different resource at a different cadence: a
	// GPU-hour rate belongs to a cluster and is declared once, a price book
	// belongs to a model and a provider and is reopened whenever a vendor
	// changes its list.
	Prices *sqlstore.PriceStore
	// Detail is the reporting replica. Optional, and unlike the gateway's it is
	// not opened at startup: the control plane only reads it, and refusing to
	// manage models because a diagnostic's database is down would trade a
	// working product for a broken one. See detailConn.
	Detail clickhouse.Config
	// PullConcurrency is how many pulls may run at once.
	PullConcurrency int
	// FileConcurrency is how many files within one pull may download at once.
	FileConcurrency int
	// AllowedOrigins is the CORS allowlist. Empty means loopback only.
	AllowedOrigins []string
	// AdminTokens guards every management route. It is deliberately separate
	// from a tenant key: a tenant may spend, and this may set the price, mint
	// keys and close an invoice.
	//
	// A set rather than one, and deliberately not roles: a shared credential
	// means one person leaving forces a rotation, and rotation invalidates what
	// the others are using. Several tokens turns that into "stop using theirs".
	//
	// Optional only on a loopback listen address. A server bound to the
	// wildcard without one would hand the whole money surface of the platform
	// to anything that can open a socket, so NewServer refuses that instead.
	AdminTokens []string
}

// Server owns the control plane's dependencies.
type Server struct {
	cfg      Config
	store    registry.Store
	blobs    blobstore.Store
	profiles *engine.Profiles
	puller   *Puller
	worker   *registry.Worker
	log      *slog.Logger
	db       *sqlstore.DB
	policies *sqlstore.PolicySource
	keys     *sqlstore.KeyStore
	quota    *sqlstore.Quota
	cost     *sqlstore.CostStore
	prices   *sqlstore.PriceStore
	detail   *detailConn
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
	if len(cfg.adminTokens()) == 0 && listensOffHost(cfg.Listen) {
		return nil, errs.InvalidArgument(
			"apiserver: listening on %s exposes tenants, keys and billing to the network, "+
				"so an admin token is required; set FLEET_ADMIN_TOKEN, or bind 127.0.0.1:8081",
			cfg.Listen)
	}
	// The database is optional. Without one the gateway, the console and the
	// pull queue all work; only tenancy is missing, and its routes say so
	// rather than answering with an empty list that reads as "no tenants".
	// A laptop with no PostgreSQL is a supported way to try Fleet.
	if cfg.DB != nil && (cfg.Policies == nil || cfg.Keys == nil || cfg.Quota == nil ||
		cfg.Cost == nil || cfg.Prices == nil) {
		return nil, errs.InvalidArgument("apiserver: Policies, Keys, Quota, Cost and Prices are required with DB")
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
		cfg: cfg, store: store, blobs: blobs, profiles: profiles,
		puller: puller, worker: worker, log: log,
		db: cfg.DB, policies: cfg.Policies, keys: cfg.Keys, quota: cfg.Quota,
		cost: cfg.Cost, prices: cfg.Prices,
		detail: &detailConn{cfg: cfg.Detail, log: log},
	}, nil
}

func (s *Server) Close() {
	// Drains the queue and lets running pulls finish; a 40 GiB pull should not
	// be abandoned because the control plane is restarting.
	s.worker.Close()
	s.detail.Close()
}

// ── storage ────────────────────────────────────────────────────

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.blobs.Info(r.Context()))
}

// engineView is the console's view of one profile. It is a projection, not the
