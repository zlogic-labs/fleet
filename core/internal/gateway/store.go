package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
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
