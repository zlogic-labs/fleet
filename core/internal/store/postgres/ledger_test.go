package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// ── the ledger ─────────────────────────────────────────────────

// A stored record is readable back with every billing dimension intact, and the
// id is available immediately — which is what makes a dispute point at one row
// rather than at a time range.
func TestLedgerAppendReturnsAReadableRow(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	ledger := NewLedger(db)
	id, err := ledger.Record(ctx, billing.Record{
		Tenant: "acme", Project: "research", KeyID: "acme/research/k1",
		Model: "qwen-7b", Endpoint: "e1", PriceBook: "qwen-7b@20260101",
		Usage: usage(1000, 200, 400),
		// The record's own amount, not one recomputed from a price book: the row
		// is a self-contained statement of what this request cost.
		Amount:     1234,
		UsageKnown: true,
		TTFT:       120 * time.Millisecond, Duration: 2 * time.Second, Streamed: true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		model    string
		prompt   int
		cached   int
		amount   int64
		streamed bool
		known    bool
	)
	err = db.pool.QueryRow(ctx, `
		SELECT model, prompt_tokens, cached_tokens, amounts_micro, streamed, usage_known
		  FROM usage_events WHERE id = $1`, id).
		Scan(&model, &prompt, &cached, &amount, &streamed, &known)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if model != "qwen-7b" || prompt != 1000 || cached != 400 || amount != 1234 || !streamed || !known {
		t.Errorf("row came back as model=%s prompt=%d cached=%d amount=%d streamed=%v known=%v",
			model, prompt, cached, amount, streamed, known)
	}
}

// An unattributed project is stored as NULL, not as an empty string. "" would
// make a report group unattributed spend under a cost centre named after
// nothing, and every rollup would have to special-case it.
func TestUnattributedDimensionsAreStoredAsNull(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	id, err := NewLedger(db).Record(ctx, billing.Record{
		Tenant: "acme", Model: "m", Usage: usage(10, 2, 0), UsageKnown: true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var project, key, book *string
	if err := db.pool.QueryRow(ctx,
		`SELECT project_id, key_id, price_book_id FROM usage_events WHERE id = $1`, id).
		Scan(&project, &key, &book); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if project != nil || key != nil || book != nil {
		t.Errorf("empty dimensions stored as %v/%v/%v, want NULL", project, key, book)
	}
}

// cached_tokens <= prompt_tokens and reasoning_tokens <= completion_tokens are
// the wire format's inclusions, not Fleet's. The constraints are what stop a
// billing bug from becoming a negative charge.
func TestLedgerRefusesImpossibleUsage(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	ledger := NewLedger(db)

	base := billing.Record{Tenant: "acme", Model: "m", Usage: usage(100, 100, 0), UsageKnown: true}
	bad := base
	bad.Usage = usage(100, 100, 500)
	if _, err := ledger.Record(ctx, bad); err == nil {
		t.Error("cached_tokens above prompt_tokens was accepted")
	}
	bad = base
	bad.Usage = openai.Usage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 600,
		CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: 500}}
	if _, err := ledger.Record(ctx, bad); err == nil {
		t.Error("reasoning_tokens above completion_tokens was accepted")
	}
	bad = base
	bad.Amount = -1
	if _, err := ledger.Record(ctx, bad); err == nil {
		t.Error("a negative amount was accepted; the ledger must never carry a credit")
	}
}

// An estimate is charged at max_tokens and later corrected. Reconciling twice
// must be a no-op, not a double refund, because two reconcilers racing is
// normal.
func TestReconcileIsIdempotent(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	ledger := NewLedger(db)

	id, err := ledger.Record(ctx, billing.Record{
		Tenant: "acme", Model: "m", Usage: usage(10, 4096, 0),
		Amount: 9999, UsageKnown: false,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	real := billing.Record{Usage: usage(10, 12, 0), Amount: 40}
	for i := 0; i < 2; i++ {
		if err := ledger.Reconcile(ctx, id, real); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var amount int64
	var known bool
	if err := db.pool.QueryRow(ctx,
		`SELECT amounts_micro, usage_known FROM usage_events WHERE id = $1`, id).
		Scan(&amount, &known); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if amount != 40 {
		t.Errorf("amount = %d after two reconciles, want 40 — the second was not a no-op", amount)
	}
	if !known {
		t.Error("usage_known is still false after a reconcile")
	}
}

// usage builds a Usage whose totals are consistent, because the ledger's
// constraints compare the details against the totals and a helper that let them
// disagree would make every test of those constraints a lie.
func usage(prompt, completion, cached int) openai.Usage {
	u := openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	if cached > 0 {
		u.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: cached}
	}
	return u
}
