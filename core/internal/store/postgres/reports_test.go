package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// ── reports ────────────────────────────────────────────────────

// The report totals match what was appended, and estimates are counted
// separately rather than being indistinguishable inside the total.
func TestSpendReportCountsEstimatesSeparately(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	ledger := NewLedger(db)

	now := time.Now()
	for _, e := range []billing.Record{
		{Tenant: "acme", Project: "research", Model: "m", Usage: usage(100, 10, 0), Amount: 500, UsageKnown: true},
		{Tenant: "acme", Project: "research", Model: "m", Usage: usage(100, 10, 60), Amount: 300, UsageKnown: true},
		{Tenant: "acme", Project: "sales", Model: "m", Usage: usage(50, 5, 0), Amount: 700, UsageKnown: false},
	} {
		e.OccurredAt = now
		if _, err := ledger.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	rows, err := db.SpendByTenant(ctx, "acme", Window{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("SpendByTenant: %v", err)
	}
	byProject := map[string]Spend{}
	for _, r := range rows {
		byProject[r.Key] = r
	}
	research := byProject["research"]
	if research.Requests != 2 || research.UnitsMicro != 800 {
		t.Errorf("research = %d requests / %d micro, want 2 / 800", research.Requests, research.UnitsMicro)
	}
	if research.CachedTokens != 60 {
		t.Errorf("research cached = %d, want 60", research.CachedTokens)
	}
	sales := byProject["sales"]
	if sales.Estimated != 1 {
		t.Errorf("sales estimated = %d, want 1 — an estimate the report cannot see is an error nobody finds", sales.Estimated)
	}
	if research.Estimated != 0 {
		t.Errorf("research estimated = %d, want 0", research.Estimated)
	}
}

// A window's end is exclusive. A report that included both ends would double
// count the boundary instant — the same bug a sliding-window limiter avoids.
func TestWindowEndIsExclusive(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	ledger := NewLedger(db)

	boundary := time.Now().Truncate(time.Second)
	if _, err := ledger.Record(ctx, billing.Record{
		Tenant: "acme", Model: "m", Usage: usage(1, 1, 0), Amount: 100, OccurredAt: boundary,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := db.SpendByTenant(ctx, "acme", Window{From: boundary, To: boundary})
	if err == nil {
		t.Errorf("an empty window was accepted; %d rows came back", len(rows))
	}
	rows, err = db.SpendByTenant(ctx, "acme", Window{From: boundary, To: boundary.Add(time.Second)})
	if err != nil {
		t.Fatalf("SpendByTenant: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a one-second window returned %d rows, want 1", len(rows))
	}
	rows, err = db.SpendByTenant(ctx, "acme", Window{From: boundary.Add(time.Second), To: boundary.Add(2 * time.Second)})
	if err != nil {
		t.Fatalf("SpendByTenant: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the window starting at the event returned %d rows; the end is exclusive", len(rows))
	}
}
