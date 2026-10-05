package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// These exercise the gateway's own wiring against a real database, not the
// store's own tests. The question is the one a deployment actually asks: set
// database.url, and does the gateway authenticate against it and enforce the
// limits stored there?
//
// They skip without FLEET_TEST_DATABASE_URL, so a laptop with no PostgreSQL
// still gets a green run.

// testLockKey must match the one internal/store/postgres uses.
//
// Both packages truncate the same tables, and `go test ./...` runs packages in
// parallel. Without a shared lock one package deletes the rows the other just
// seeded and the failure reads like a constraint bug. An advisory lock held for
// the whole binary is the cheapest way to make them take turns.
const testLockKey = 0xF1EE7

func TestMain(m *testing.M) {
	url := os.Getenv("FLEET_TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := sqlstore.Open(ctx, sqlstore.Config{URL: url, ConnectTimeout: 10 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach the test database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Dedicated connection: an advisory lock belongs to the session that took
	// it, so one taken on a pooled connection would be released by whoever
	// borrowed that connection next.
	conn, err := db.Pool().Acquire(ctx)
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

func wireDB(t *testing.T) *sqlstore.DB {
	t.Helper()
	url := os.Getenv("FLEET_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is unset; skipping the wired database tests")
	}
	ctx := context.Background()
	db, err := sqlstore.Open(ctx, sqlstore.Config{URL: url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// The whole point of the wiring: a key that exists only in PostgreSQL
// authenticates, and one that does not exist does not.
func TestGatewayAuthenticatesAgainstTheDatabase(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)

	tenant := seedTenant(t, db, "acme", 100, 100_000)
	created := seedKey(t, db, tenant, "research", "laptop")

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := buildWired(t, db, up.URL)

	if w := post(h, created, req); w.Code != 200 {
		t.Fatalf("a key stored in PostgreSQL was refused: %d %s", w.Code, w.Body)
	}
	if w := post(h, "sk-fleet-invented", req); w.Code != 401 {
		t.Errorf("an invented key was accepted: %d", w.Code)
	}
}

// The limit in force is the one in the database, not a default. 3 rpm against a
// stub engine: the fourth request must be refused, and the engine must have
// seen exactly three.
func TestGatewayEnforcesTheLimitStoredInTheDatabase(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)

	tenant := seedTenant(t, db, "acme", 3, 100_000)
	created := seedKey(t, db, tenant, "research", "laptop")

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := buildWired(t, db, up.URL)

	for i := 1; i <= 3; i++ {
		if w := post(h, created, req); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := post(h, created, req); w.Code != 429 {
		t.Errorf("the fourth request got %d, want 429 — the database limit is not in force", w.Code)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("engine saw %d calls, want 3 — a refused request still reached the GPU", got)
	}
}

// A project partition stored in the database divides the tenant's envelope,
// through the same limiter code the config path uses.
func TestGatewayEnforcesTheProjectPartitionFromTheDatabase(t *testing.T) {
	db := wireDB(t)
	ctx := context.Background()
	truncateWired(t, db)

	tenant := seedTenant(t, db, "acme", 100, 1_000_000)
	created := seedKey(t, db, tenant, "research", "laptop")
	if _, err := db.Pool().Exec(ctx,
		`UPDATE projects SET request_limit = 2 WHERE tenant_id = $1 AND name = 'research'`,
		tenant); err != nil {
		t.Fatalf("set the partition: %v", err)
	}

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := buildWired(t, db, up.URL)

	for i := 1; i <= 2; i++ {
		if w := post(h, created, req); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := post(h, created, req); w.Code != 429 {
		t.Errorf("the third request got %d, want 429 — the project partition is not in force", w.Code)
	}
}

// Two tenants, two keys, one database: neither can spend the other's budget and
// neither can use the other's credential.
func TestTwoTenantsShareOneDatabase(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)

	acme := seedTenant(t, db, "acme", 1, 100_000)
	acmeKey := seedKey(t, db, acme, "research", "a")
	globex := seedTenant(t, db, "globex", 100, 100_000)
	globexKey := seedKey(t, db, globex, "sales", "g")

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := buildWired(t, db, up.URL)

	if w := post(h, acmeKey, req); w.Code != 200 {
		t.Fatalf("acme: %d", w.Code)
	}
	if w := post(h, acmeKey, req); w.Code != 429 {
		t.Errorf("acme spent past its own envelope: %d", w.Code)
	}
	// The whole reason keys are stored hashed and looked up per row: acme's
	// budget being exhausted must not touch globex's.
	if w := post(h, globexKey, req); w.Code != 200 {
		t.Errorf("globex was refused because acme hit its limit: %d", w.Code)
	}
}

func buildWired(t *testing.T, db *sqlstore.DB, engineURL string) http.Handler {
	t.Helper()
	cfg := Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true},
		Database:  DatabaseConfig{URL: "postgres://configured-but-unused"},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: engineURL}},
	}
	h, _, err := Build(cfg, db, detail.Nop{}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return h
}

func truncateWired(t *testing.T, db *sqlstore.DB) {
	t.Helper()
	// Every table a rule can live in belongs in the list. price_books cost a
	// debugging session: a price written by one test stayed in force for the
	// next, and a test asserting "this model has no price" quietly failed.
	// budget_rules cost the same one more time, for the same reason — a rule
	// left behind by another test's tenant is a limit this gateway never set,
	// and it arrives as a 402 that has nothing to do with the code under test.
	// The two test packages share a database, so "another test" includes tests
	// in a package this one has never heard of.
	if _, err := db.Pool().Exec(context.Background(),
		"TRUNCATE api_keys, projects, tenants, usage_events, price_books, "+
			"budget_rules, spend_counters, rate_counters CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func seedTenant(t *testing.T, db *sqlstore.DB, id string, rpm, tpm int) string {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO tenants (id, name, request_limit, token_limit, active)
		 VALUES ($1, $2, $3, $4, true)`, id, id, rpm, tpm); err != nil {
		t.Fatalf("seed tenant %s: %v", id, err)
	}
	return id
}

func seedKey(t *testing.T, db *sqlstore.DB, tenant, project, label string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO projects (id, tenant_id, name) VALUES ($1, $2, $3)`,
		tenant+"/"+project, tenant, project); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	key := "sk-fleet-" + tenant + "-" + project + "-" + label
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO api_keys (id, tenant_id, project_id, key_hash, key_prefix, label)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant+"/"+project+"/"+label, tenant, tenant+"/"+project,
		sqlstore.HashKey(key), label, label); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return key
}
