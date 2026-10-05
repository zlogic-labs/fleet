package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Where the gateway gets its tenants, keys and limits from.
//
// Separate from server.go because these are the two ways to be configured —
// all in memory, or backed by PostgreSQL — and both answers have to be right.
// The in-process shape is what a laptop runs; the database shape is what a
// deployment runs. Keeping them side by side makes the difference a readable
// few lines rather than a branch scattered through the startup path.

func keyStore(cfg Config, db *sqlstore.DB) (authn.KeyStore, error) {
	if !cfg.Auth.Required {
		return nil, nil
	}
	if db != nil {
		// Cached because a lookup happens on every request. The cache is
		// bounded and short-lived; see postgres.KeyStore for what a stale
		// entry costs.
		return sqlstore.NewKeyStore(db, sqlstore.NewTTLCache(30*time.Second, 4096)), nil
	}
	if len(cfg.Auth.Keys) == 0 {
		return nil, nil
	}
	store := authn.NewMemory()
	for i, spec := range cfg.Auth.Keys {
		p, err := authn.ParsePrincipal(spec)
		if err != nil {
			return nil, fmt.Errorf("auth.keys[%d]: %w", i, err)
		}
		// The key id is the credential. A real deployment mints random
		// strings; here the operator names them, which is what makes a
		// configuration file reviewable and a revoked key identifiable in a
		// log.
		//
		// Scoped by tenant and project rather than by key id alone, because two
		// tenants may both name a key "admin" and one tenant must not be able
		// to revoke or impersonate the other's.
		store.Put(p.Tenant+"/"+p.Project+"/"+p.KeyID, p, time.Time{})
	}
	return store, nil
}

// billingFor returns the price source and the ledger, both nil without a
// database.
//
// Nil is what makes "keep no ledger" a supported configuration rather than a
// crash: the handler treats a nil recorder as a deployment that does not bill,
// so a laptop serves traffic with no database and a production deployment
// serves the same code with one. The failure this avoids is the interesting
// one — a gateway that recorded nothing while reporting itself healthy would
// look exactly like a gateway that had billed correctly.
func billingFor(cfg Config, db *sqlstore.DB) (billing.PricerSource, billing.Recorder) {
	if db == nil {
		return nil, nil
	}
	return sqlstore.NewPriceStore(db, cfg.Database.PriceRefresh), sqlstore.NewLedger(db)
}

// budgetFor returns the spend limiter, nil without a database.
//
// Nil here means every tenant is uncapped, which is a supported configuration
// rather than a missing feature: a deployment can run Fleet on rate limits
// alone and turn budgets on per tenant later without a restart, because the
// budgets are read per reservation rather than held in a compiled-in table.
func budgetFor(db *sqlstore.DB) quota.Limiter {
	if db == nil {
		return nil
	}
	return sqlstore.NewQuota(db)
}

// openDatabase connects when one is configured, and returns nil when it is not.
//
// Nil is a supported state rather than a fallback: a gateway with no database
// keeps its tenants, keys and limits in memory and loses them on restart, which
// is what a laptop wants and what a production deployment must not have. The
// configuration decides which, and the process does not second-guess it.
//
// A configured database that will not connect stops the process. Carrying on
// with an empty in-memory store would refuse every tenant while reporting
// itself healthy, which is the worst of both: an outage that looks like a
// healthy empty deployment.

func openDatabase(ctx context.Context, cfg Config, log *slog.Logger) (*sqlstore.DB, error) {
	if cfg.Database.URL == "" {
		return nil, nil
	}
	db, err := sqlstore.Open(ctx, sqlstore.Config{URL: cfg.Database.URL})
	if err != nil {
		return nil, err
	}
	if cfg.Database.Migrate {
		if err := db.Migrate(ctx); err != nil {
			db.Close()
			return nil, err
		}
		log.Info("applied the database schema")
	}
	log.Info("using the database for tenants, keys and limits")
	return db, nil
}
