package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func insertUsage(t *testing.T, db *DB, tenant, model string, prompt, completion int, cached any, micro int, at time.Time) {
	t.Helper()
	mustExec(t, db, `INSERT INTO usage_events
		(tenant_id, project_id, model, endpoint_id, prompt_tokens, completion_tokens,
		 cached_tokens, amounts_micro, occurred_at)
		VALUES ($1, $2, $3, 'fleet/'||$3, $4, $5, $6, $7, $8)`,
		tenant, tenant+"/research", model, prompt, completion, cached, micro, at)
}

func TestTheLedgerTallyCountsWhatWasWritten(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := from.Add(48 * time.Hour)
	// Two rows with a breakdown and one without, because the breakdown is
	// nullable and a SUM over a group where every row omitted it is NULL rather
	// than zero -- which fails the scan on a month that is perfectly healthy.
	insertUsage(t, db, "acme", "m1", 100, 20, 40, 314, at)
	insertUsage(t, db, "acme", "m1", 100, 20, 40, 314, at.Add(time.Second))
	insertUsage(t, db, "globex", "m2", 50, 10, nil, 100, at.Add(2*time.Second))

	w := Window{From: from, To: from.AddDate(0, 1, 0)}
	got, err := db.UsageTally(ctx, w)
	if err != nil {
		t.Fatalf("UsageTally: %v", err)
	}
	want := billing.Tally{Records: 3, PromptTokens: 250, CompletionTokens: 50, CachedTokens: 80, AmountMicro: 728}
	if got != want {
		t.Fatalf("tally is %+v, want %+v", got, want)
	}
}

func TestTheLedgerGroupsAreKeyedTheWayTheReplicaKeysThem(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := from.Add(48 * time.Hour)
	insertUsage(t, db, "acme", "m1", 100, 20, 40, 314, at)
	insertUsage(t, db, "acme", "m1", 100, 20, 40, 314, at.Add(time.Second))
	insertUsage(t, db, "globex", "m2", 50, 10, nil, 100, at.Add(2*time.Second))

	got, err := db.UsageByGroup(ctx, Window{From: from, To: from.AddDate(0, 1, 0)})
	if err != nil {
		t.Fatalf("UsageByGroup: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	// The key is the shared spelling, not whatever this query felt like
	// producing: the replica builds the same string, and a separator that
	// differed between the two would report every group as disagreeing.
	if got[0].Key != billing.GroupKey("acme", "m1") {
		t.Errorf("first key is %q, want %q", got[0].Key, billing.GroupKey("acme", "m1"))
	}
	if got[1].Key != billing.GroupKey("globex", "m2") {
		t.Errorf("second key is %q, want %q", got[1].Key, billing.GroupKey("globex", "m2"))
	}
	if got[0].Records != 2 || got[0].AmountMicro != 628 {
		t.Errorf("acme/m1 is %+v, want 2 records and 628 micro", got[0].Tally)
	}
	if got[1].CachedTokens != 0 {
		t.Errorf("a group with no breakdown reported %d cached tokens", got[1].CachedTokens)
	}
}

func TestTheTallyExcludesTheClosingInstant(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	insertUsage(t, db, "acme", "m1", 10, 1, nil, 10, to.Add(-time.Second))
	insertUsage(t, db, "acme", "m1", 10, 1, nil, 10, to)

	got, err := db.UsageTally(ctx, Window{From: from, To: to})
	if err != nil {
		t.Fatalf("UsageTally: %v", err)
	}
	// Half-open, the same convention the reports and the rate limiter use. A
	// window that included both ends would count the boundary instant twice, and
	// the reconciliation would then find a one-row disagreement every month.
	if got.Records != 1 {
		t.Fatalf("the closing instant was counted: %+v", got)
	}
}

func TestTheTallyRefusesAnUnusableWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.UsageTally(ctx, Window{}); err == nil {
		t.Fatal("a window with no ends was accepted")
	}
	if _, err := db.UsageByGroup(ctx, Window{From: time.Now(), To: time.Now()}); err == nil {
		t.Fatal("an empty window was accepted")
	}
}
