package postgres

import (
	"context"
	"testing"
	"time"
)

// An unfiltered report must not behave like a filtered one with an empty key.
//
// This is not a hypothetical trap. SpendByScope exists because the obvious
// spelling of it — SpendByTenant(ctx, "", window) — looks like "every tenant"
// and is not: the predicate is an equality test, so the empty string selects
// only rows whose tenant genuinely is empty. A report built that way returns
// nothing on a healthy deployment and reads as "no spend yet".
func TestSpendByScopeCoversEveryScopeNotJustEmptyOnes(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := Window{From: from, To: from.AddDate(0, 1, 0)}

	for _, e := range []struct {
		tenant, project string
		micro           int
	}{
		{"acme", "acme/research", 700},
		{"acme", "acme/batch", 300},
		{"globex", "", 500},
	} {
		var project any = e.project
		if e.project == "" {
			project = nil // unattributed spend, which must still be reported
		}
		mustExec(t, db, `INSERT INTO usage_events
			(tenant_id, project_id, model, endpoint_id, amounts_micro, occurred_at)
			VALUES ($1, $2, 'm1', 'demo', $3, $4)`,
			e.tenant, project, e.micro, from.Add(24*time.Hour))
	}

	// The comparison that makes this a test rather than a transcription.
	filtered, err := db.SpendByTenant(ctx, "", w)
	if err != nil {
		t.Fatalf("SpendByTenant: %v", err)
	}
	if len(filtered) != 0 {
		t.Fatalf("the premise no longer holds: an empty tenant matched %d rows", len(filtered))
	}

	got, err := db.SpendByScope(ctx, w)
	if err != nil {
		t.Fatalf("SpendByScope: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d scopes, want 3: %+v", len(got), got)
	}
	if got[0].Key != "acme/research" || got[0].UnitsMicro != 700 {
		t.Errorf("first row is %s/%d, want acme/research/700 — the report is not ordered by spend",
			got[0].Key, got[0].UnitsMicro)
	}

	// Unattributed spend keys on the tenant rather than vanishing, because a
	// report that silently drops rows is worse than one that labels them.
	for _, r := range got {
		if r.Key == "globex" {
			if r.UnitsMicro != 500 {
				t.Errorf("unattributed spend for globex is %d, want 500", r.UnitsMicro)
			}
			return
		}
	}
	t.Error("globex's unattributed spend is missing from the report")
}
