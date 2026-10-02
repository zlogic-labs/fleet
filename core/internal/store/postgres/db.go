// Package postgres is Fleet's durable store.
//
// It implements the interfaces the gateway and control plane already declare —
// authn.KeyStore, billing's price book source, the ledger writer — against one
// PostgreSQL database, so that a deployment which loses its process no longer
// loses its tenants, its prices, or its ledger.
//
// Scope is deliberately narrow. This is not an ORM and there is no query
// builder: every statement here is hand-written SQL that can be read next to
// the schema it targets. The five tables in schema.sql are the whole data
// model, and a reader who knows them can predict every query below.
package postgres

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schema is applied on Migrate. Embedding it rather than shipping a separate
// .sql file means a binary cannot be started without the DDL that defines the
// tables it will use.
//
//go:embed schema.sql
var schema string

// DB owns the connection pool.
//
// One pool for the process rather than one per subsystem: pgx multiplexes
// connections, and a second pool is a second thing to size, monitor and leak.
type DB struct {
	pool *pgxpool.Pool
}

// Config is everything needed to reach the database.
type Config struct {
	// URL is a libpq/pgx connection string. A URL rather than discrete
	// fields because that is what every managed provider hands out, and an
	// operator should be able to paste what their dashboard shows.
	URL string
	// MaxConns caps concurrent connections. Left at pgx's default unless set,
	// because a wrong number here is a production outage and the default is
	// chosen to be safe.
	MaxConns int32
	// ConnectTimeout bounds the initial connection, not each query. A database
	// that is slow to accept but healthy should not stop the gateway booting.
	ConnectTimeout time.Duration
}

// Open connects and verifies the connection.
//
// The ping is not optional. A pool that was handed back without one would fail
// on the first tenant's request instead, and the operator would be reading a
// 500 from the gateway rather than a connection error from the process that
// could not reach Postgres.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("postgres: connection URL is empty")
	}
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse connection URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.ConnectTimeout > 0 {
		pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Migrate applies the schema.
//
// Idempotent, because every statement is CREATE ... IF NOT EXISTS and a
// deployment that applies it on every boot should not need to know whether it
// already ran. It is not a migration tool and does not pretend to be: there is
// no down path, no version table, and no way to alter a column. Changing the
// schema means writing a new statement and applying it deliberately.
//
// Note what idempotence does not give: an existing table with an old shape is
// left exactly as it is. IF NOT EXISTS means "create if absent", not "make
// match", so a column added to schema.sql after a deployment started does not
// appear until someone alters the table. That is a deliberate trade for having
// no migration tool at all — an ALTER that silently rewrites a ledger table is
// worse than an operator running one by hand.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("postgres: apply schema: %w", err)
	}
	return nil
}

func (db *DB) Close() {
	if db.pool != nil {
		db.pool.Close()
	}
}

// Pool exposes the underlying pool for the packages that need a transaction.
//
// Exposed rather than wrapped in a dozen pass-through methods: every caller
// that wants one already needs pgx's Query/QueryRow signatures, and a wrapper
// would be a second set of signatures to keep in step.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping reports whether the database is reachable, for a readiness probe.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// inTx runs fn inside a transaction, rolling back on error or panic.
//
// The rollback-on-panic is the part that is easy to leave out and expensive to
// miss: a handler that panics after a partial write would otherwise leave the
// connection holding an open transaction, and every later query on that pooled
// connection inherits it.
func (db *DB) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// A failed rollback on a connection that is already broken is not
			// actionable; pgx has already discarded the connection by then.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	committed = true
	return nil
}
