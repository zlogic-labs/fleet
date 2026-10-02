package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// These tests run against a real PostgreSQL.
//
// Not because a fake would be hard, but because most of what can go wrong here
// is SQL that a fake cannot see: a constraint that does not fire, a NULL that
// scans into the wrong type, a partial index that permits two open price books.
// A test double for the database tests the double.
//
// They skip when FLEET_TEST_DATABASE_URL is unset, so `go test ./...` on a
// laptop with no database is still green rather than failing for a reason that
// has nothing to do with the change.

// testLockKey serialises the packages that share one test database.
//
// internal/gateway also truncates these tables, and `go test ./...` runs
// packages in parallel — so without this the two interleave, one truncates the
// rows the other just seeded, and the failure looks like a constraint bug. An
// advisory lock held for the whole run is the cheapest thing that makes them
// take turns.
//
// 0xF1EE7 spells "fleet" and is arbitrary; what matters is that both packages
// use the same number.
const testLockKey = 0xF1EE7

// TestMain holds the lock for the life of this test binary.
func TestMain(m *testing.M) {
	url := os.Getenv("FLEET_TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := Open(ctx, Config{URL: url, ConnectTimeout: 10 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach the test database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// A dedicated connection, because an advisory lock belongs to the session
	// that took it: a lock taken on a pooled connection and then returned to
	// the pool would be released by whoever next borrowed it.
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot acquire a connection: %v\n", err)
		os.Exit(1)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, testLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "cannot take the test lock: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, testLockKey)
	os.Exit(code)
}

func testDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("FLEET_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is unset; skipping the database tests")
	}
	ctx := context.Background()
	db, err := Open(ctx, Config{URL: url, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// truncate empties the tables a test touched.
//
// TRUNCATE ... CASCADE rather than DELETE: it does not fire per-row triggers or
// bloat the table, and the tests are the only thing using this database.
func truncate(t *testing.T, db *DB, tables ...string) {
	t.Helper()
	q := "TRUNCATE " + tableList(tables) + " CASCADE"
	if _, err := db.pool.Exec(context.Background(), q); err != nil {
		t.Fatalf("truncate %s: %v", tableList(tables), err)
	}
}

func tableList(tables []string) string {
	out := ""
	for i, tb := range tables {
		if i > 0 {
			out += ", "
		}
		out += tb
	}
	return out
}
