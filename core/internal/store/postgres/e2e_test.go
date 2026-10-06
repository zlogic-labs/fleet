package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// ── end to end ─────────────────────────────────────────────────

// The path the gateway actually takes: a request arrives under a key, the
// limits come from the database, the price comes from the database, and the
// settlement lands in the ledger.
func TestGatewayPathAgainstADatabase(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events", "price_books", "api_keys", "projects", "tenants")
	ctx := context.Background()

	keys := NewKeyStore(db, nil)
	src := NewPolicySource(db, ratelimit.Policy{RequestsPerMinute: 100})
	if err := src.CreateTenant(ctx, TenantRow{ID: "acme", Name: "Acme", RequestLimit: 10, Active: true}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	key, _, err := keys.CreateKey(ctx, "acme", "research", "laptop")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	prices := NewPriceStore(db, time.Minute)
	if _, err := prices.PutPrice(ctx,
		billing.Price{Model: "qwen-7b", Rate: billing.Rate{Input: 1000, Output: 2000, Cached: 100}},
		time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("PutPrice: %v", err)
	}
	pricer, err := prices.Pricer(ctx)
	if err != nil {
		t.Fatalf("Pricer: %v", err)
	}

	// authenticate
	p, ok := keys.Lookup(ctx, key)
	if !ok {
		t.Fatal("the key did not resolve")
	}

	// reserve
	limiter := ratelimit.NewMemory(func(s ratelimit.Scope) ratelimit.Policies {
		return src.Policies(s)
	})
	scope := ratelimit.Scope{Tenant: p.Tenant, Project: p.Project}
	res, err := limiter.Reserve(ctx, ratelimit.Request{Scope: scope, Tokens: 4096})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// settle and charge
	u := usage(1000, 200, 0)
	amount, err := pricer.Charge("qwen-7b", "", u)
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	limiter.Settle(ctx, res, u.TotalTokens)

	ledger := NewLedger(db)
	id, err := ledger.Record(ctx, billing.Record{
		Tenant: p.Tenant, Project: p.Project, KeyID: p.KeyID,
		Model: "qwen-7b", PriceBook: prices.BookID("qwen-7b", ""),
		Usage: u, Amount: amount, UsageKnown: true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id == 0 {
		t.Error("the ledger returned no row id")
	}
	if amount <= 0 {
		t.Errorf("charged %d, want a positive amount", amount)
	}
}
