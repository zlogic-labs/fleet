package postgres

import (
	"context"
	"testing"
	"time"
)

// ── the ledger ─────────────────────────────────────────────────

// An appended event is readable back with every billing dimension intact, and
// the id is available immediately — which is what makes a dispute point at one
// row rather than at a time range.
func TestLedgerAppendReturnsAReadableRow(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	ledger := NewLedger(db)
	id, err := ledger.Append(ctx, Event{
		Tenant: "acme", Project: "research", KeyID: "acme/research/k1",
		Model: "qwen-7b", Endpoint: "e1", PriceBook: "qwen-7b@20260101",
		PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 400,
		AmountMicro: 1234, UsageKnown: true,
		TTFT: 120 * time.Millisecond, Duration: 2 * time.Second, Streamed: true,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
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

// cached_tokens <= prompt_tokens and reasoning_tokens <= completion_tokens are
// the wire format's inclusions, not Fleet's. The constraints are what stop a
// billing bug from becoming a negative charge.
func TestLedgerRefusesImpossibleUsage(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()
	ledger := NewLedger(db)

	base := Event{Tenant: "acme", Model: "m", PromptTokens: 100, CompletionTokens: 100}
	bad := base
	bad.CachedTokens = 500
	if _, err := ledger.Append(ctx, bad); err == nil {
		t.Error("cached_tokens above prompt_tokens was accepted")
	}
	bad = base
	bad.ReasoningTokens = 500
	if _, err := ledger.Append(ctx, bad); err == nil {
		t.Error("reasoning_tokens above completion_tokens was accepted")
	}
	bad = base
	bad.AmountMicro = -1
	if _, err := ledger.Append(ctx, bad); err == nil {
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

	id, err := ledger.Append(ctx, Event{
		Tenant: "acme", Model: "m", PromptTokens: 10, CompletionTokens: 4096,
		AmountMicro: 9999, UsageKnown: false,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	real := Event{PromptTokens: 10, CompletionTokens: 12, AmountMicro: 40}
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
