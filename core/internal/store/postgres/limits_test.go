package postgres

import (
	"context"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// ── limits ─────────────────────────────────────────────────────

// The limiter's two levels come from the database unchanged, so a gateway
// backed by Postgres and one backed by FLEET_RATE_TENANTS enforce the same
// arithmetic.
func TestPoliciesComeFromTheDatabase(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	keys := NewKeyStore(db, nil)
	src := NewPolicySource(db, ratelimit.Policy{RequestsPerMinute: 100})

	if err := src.CreateTenant(ctx, TenantRow{
		ID: "acme", Name: "Acme", RequestLimit: 300, TokenLimit: 100_000, Active: true,
	}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, _, err := keys.CreateKey(ctx, "acme", "research", "laptop"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// Recreated with real limits: CreateKey already made the project with none,
	// and the partition under test has to be a deliberate number.
	if _, err := db.pool.Exec(ctx,
		`UPDATE projects SET request_limit = 60, token_limit = 20000
		  WHERE tenant_id = 'acme' AND name = 'research'`); err != nil {
		t.Fatalf("set the project partition: %v", err)
	}

	p := src.Policies(ratelimit.Scope{Tenant: "acme", Project: "research"})
	if p.Envelope.RequestsPerMinute != 300 {
		t.Errorf("envelope = %d, want 300", p.Envelope.RequestsPerMinute)
	}
	if p.Partition.RequestsPerMinute != 60 {
		t.Errorf("partition = %d, want 60", p.Partition.RequestsPerMinute)
	}

	// A tenant nobody declared runs under the server defaults rather than
	// unlimited — "no row" and "no limit" are different answers.
	q := src.Policies(ratelimit.Scope{Tenant: "nobody"})
	if q.Envelope.RequestsPerMinute != 100 {
		t.Errorf("undeclared tenant = %d, want the server default 100", q.Envelope.RequestsPerMinute)
	}
}

// A project partition above its tenant's envelope can never bind. The write
// path refuses it, because a CHECK constraint cannot compare two tables.
func TestProjectLimitAboveEnvelopeIsRefused(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	src := NewPolicySource(db, ratelimit.Policy{})
	if err := src.CreateTenant(ctx, TenantRow{ID: "acme", Name: "Acme", RequestLimit: 100, Active: true}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	err := src.CreateProject(ctx, ProjectRow{
		ID: "acme/big", TenantID: "acme", Name: "big", RequestLimit: 300,
	})
	if err == nil {
		t.Fatal("a project partition above its tenant envelope was accepted")
	}
}
