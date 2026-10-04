// Package clickhouse stores Fleet's usage detail.
//
// It is a replica. Postgres is the ledger; this is what you read when you want
// to ask a question the ledger is the wrong shape for. Every number here can be
// recomputed from usage_events, so this package may lose data and may be
// unavailable, and neither may affect whether a request is served.
//
// The schema's ordering key is the whole design. ClickHouse does not have
// indexes in the relational sense: a query reads the primary key's prefix, and
// everything else is a full scan. So the question "which ordering makes the real
// questions cheap" has to be answered before any column is added, not after.
package clickhouse

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config addresses a ClickHouse server.
type Config struct {
	URL      string
	Database string
	User     string
	Password string
}

// Store is a connection to the detail database.
type Store struct {
	conn driver.Conn
	db   string
}

// Open connects and verifies the server answers.
//
// The ping is not optional: a Store that connects lazily turns a wrong URL into
// a failure on the first request's settlement rather than at startup, and the
// whole point of the mirror is that it must never be on that path.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("clickhouse: no url")
	}
	addr, err := splitURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	db := cfg.Database
	if db == "" {
		db = "default"
	}

	conn, err := dial(ctx, addr, db, cfg)
	if err != nil && db != "default" && isUnknownDatabase(err) {
		// The handshake carries the database, so a database that has not been
		// created yet fails the connection -- while creating it is Migrate's job.
		// Fall back to the default database, which always exists, and let
		// Migrate create the target. Without this a fresh install cannot start.
		conn, err = dial(ctx, addr, "default", cfg)
	}
	if err != nil {
		return nil, err
	}
	return &Store{conn: conn, db: db}, nil
}

func dial(ctx context.Context, addr, db string, cfg Config) (driver.Conn, error) {
	opts := &chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{
			Database: db,
			Username: cfg.User,
			Password: cfg.Password,
		},
		// HTTP, not the native protocol the driver defaults to. A URL implies
		// HTTP, and the two protocols disagree about what a port means: the
		// driver would otherwise open the native protocol against the HTTP port
		// and fail with something that names neither.
		Protocol: chdriver.HTTP,
		// Compression costs a little CPU and saves a lot of bandwidth. The
		// batches are thousands of similar rows, which is the case lz4 is good
		// at.
		Compression: &chdriver.Compression{Method: chdriver.CompressionLZ4},
		Settings: chdriver.Settings{
			// The mirror is written once and read rarely; letting the server
			// background its own merges would compete with the inference
			// engines for the same CPU.
			"background_pool_size": 4,
		},
		DialTimeout: 5 * time.Second,
	}
	conn, err := chdriver.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return conn, nil
}

// isUnknownDatabase reports whether the server refused because of a database
// that is not there, rather than because of a wrong password.
//
// The distinction matters: the first is a fresh install and the second is a
// misconfiguration, and retrying the second against the default database would
// turn a clear authentication failure into a confusing one.
func isUnknownDatabase(err error) bool {
	return strings.Contains(err.Error(), "UNKNOWN_DATABASE") ||
		strings.Contains(err.Error(), "does not exist")
}

// splitURL turns a URL into the host:port the driver wants.
//
// It matters that this happens here: the setting is FLEET_CLICKHOUSE_URL and the
// install script writes a URL, so an operator's first attempt would otherwise
// fail with "too many colons in address", which says nothing about what to do
// next.
//
// A bare host:port is accepted too.
func splitURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("clickhouse: %q is not a url: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("clickhouse: %q has no host", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("clickhouse: %q must be http or https", raw)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	if u.Scheme == "https" {
		// The driver's TLS configuration is a tls.Config rather than a flag, and
		// building one here would mean inventing a certificate policy for a
		// replica of a table that holds no credential. Refusing says the same
		// thing without guessing.
		return "", fmt.Errorf(
			"clickhouse: %q has no port; this build speaks plain http only, so an https url needs a port and a proxy in front of it", raw)
	}
	return u.Host + ":8123", nil
}

// Pool returns the underlying connection, for callers that need a query.
func (s *Store) Pool() driver.Conn { return s.conn }

// Close releases the connection.
func (s *Store) Close() error { return s.conn.Close() }

// Exec runs a statement, used for DDL.
func (s *Store) Exec(ctx context.Context, q string) error {
	if err := s.conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	return nil
}

// Query runs a statement that returns rows.
func (s *Store) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	rows, err := s.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return rows, nil
}

// QueryRow runs a statement expected to return one row.
func (s *Store) QueryRow(ctx context.Context, q string, args ...any) driver.Row {
	return s.conn.QueryRow(ctx, q, args...)
}
