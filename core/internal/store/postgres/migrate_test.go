package postgres

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The limit columns must reach a database that predates them.
//
// This is not hypothetical: it is what the `fleet` database on a real deployment
// had. CREATE TABLE IF NOT EXISTS is a no-op on a table that already exists, so
// the database kept its old shape and every query naming request_limit failed.
// It was not only a 500 — the gateway's policy lookup is the same query and
// falls back to server defaults when it errors, so the deployment looked
// healthy and enforced the wrong limits. Nothing announces that failure, which
// is why it needs a test rather than a code review.
//
// The statements are read out of schema.sql rather than copied, so this cannot
// pass while the file it is meant to check drifts away from it. They run in a
// scratch schema because the test database is shared with the rest of the
// package and dropping tenants from it would take the other fixtures with it.
func TestTheLimitColumnsReachAnOldDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	scratch := "fleet_migrate_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := db.Pool().Exec(ctx, `CREATE SCHEMA `+scratch); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool().Exec(context.Background(), `DROP SCHEMA IF EXISTS `+scratch+` CASCADE`)
	})

	// One dedicated connection, and only that one, ever sees the scratch
	// schema. Running this through db.Pool() would set the search_path on
	// whichever connection the pool happened to hand out and then return it,
	// and every later test in this package that borrowed it would be looking
	// for its tables in a schema that no longer exists.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// SET is session state and handing a connection back to the pool does not
	// undo it. Without this reset the next test to be given this connection
	// would inherit a search_path pointing at a schema that is about to be
	// dropped, and see no tables at all.
	defer func() {
		_, _ = conn.Exec(context.Background(), `RESET search_path`)
		conn.Release()
	}()
	if _, err := conn.Exec(ctx, `SET search_path TO `+scratch); err != nil {
		t.Fatalf("search_path: %v", err)
	}
	for _, stmt := range legacySchema {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("build the old shape: %v\n%s", err, stmt)
		}
	}

	for _, stmt := range limitColumnStatements(t) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("the shipped migration failed: %v\n%s", err, stmt)
		}
	}

	// The columns exist and default to zero, which means unlimited — the right
	// answer for a tenant that was never given a limit.
	var req, tok int
	if err := conn.QueryRow(ctx,
		`SELECT request_limit, token_limit FROM tenants`).Scan(&req, &tok); err != nil {
		t.Fatalf("a migrated tenants table must still answer the policy query: %v", err)
	}
	if req != 0 || tok != 0 {
		t.Errorf("limits defaulted to %d/%d, want 0/0 (zero means unlimited)", req, tok)
	}

	// The replaced column is gone rather than left for someone to keep reading.
	if _, err := conn.Exec(ctx, `SELECT budget_units FROM tenants`); err == nil {
		t.Error("budget_units still answers; it means something the code no longer reads")
	}
}

var legacySchema = []string{
	`CREATE TABLE tenants (
		id           text PRIMARY KEY,
		name         text        NOT NULL,
		budget_units bigint      NOT NULL DEFAULT 0,
		active       boolean     NOT NULL DEFAULT true,
		created_at   timestamptz NOT NULL DEFAULT now())`,
	`CREATE TABLE projects (
		id           text PRIMARY KEY,
		tenant_id    text        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
		name         text        NOT NULL,
		budget_units bigint      NOT NULL DEFAULT 0,
		created_at   timestamptz NOT NULL DEFAULT now())`,
	`INSERT INTO tenants (id, name, budget_units) VALUES ('legacy', 'Legacy', 500)`,
}

var statementSplit = regexp.MustCompile(`(?m)^\s*ALTER TABLE\s+(tenants|projects)\b`)

// limitColumnStatements pulls the ALTERs for the two tables out of the shipped
// schema, so the test cannot drift from the file.
func limitColumnStatements(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	text := string(raw)
	locs := statementSplit.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		t.Fatal("schema.sql declares no ALTER TABLE for tenants or projects, so an " +
			"existing database would never acquire the limit columns")
	}
	out := make([]string, 0, len(locs))
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if stmt := strings.TrimSpace(text[loc[0]:end]); stmt != "" {
			out = append(out, strings.TrimSuffix(stmt, ";"))
		}
	}
	return out
}
