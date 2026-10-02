package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// ── prices ─────────────────────────────────────────────────────

// Only the book in force is loaded, and a new price supersedes the old one
// rather than coexisting with it.
func TestOnlyTheEffectivePriceBookIsLoaded(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()

	store := NewPriceStore(db, time.Minute)
	old := billing.Price{Model: "qwen-7b", Rate: billing.Rate{Input: 1000, Output: 2000, Cached: 100}}
	if _, err := store.PutPrice(ctx, old, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("put old price: %v", err)
	}
	if _, err := store.PutPrice(ctx,
		billing.Price{Model: "qwen-7b", Rate: billing.Rate{Input: 3000, Output: 6000, Cached: 300}},
		time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("put new price: %v", err)
	}

	pricer, err := store.Pricer(ctx)
	if err != nil {
		t.Fatalf("Pricer: %v", err)
	}
	// 1M input tokens at 3000 micro/unit is 3_000_000_000 micro-units.
	got, err := pricer.Charge("qwen-7b", openai.Usage{PromptTokens: 1_000_000})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if want := billing.Amount(3000 * billing.MicroPerUnit); got != want {
		t.Errorf("charged %d, want %d — the superseded book is still in force", got, want)
	}
}

// A cached rate above the fresh input rate is refused by a CHECK constraint and
// by billing.Price.Validate. Both must agree, or an import can write a price
// the gateway will then reject at request time.
func TestCachedRateAboveInputIsRefused(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()

	store := NewPriceStore(db, time.Minute)
	bad := billing.Price{Model: "m", Rate: billing.Rate{Input: 100, Output: 200, Cached: 500}}
	if _, err := store.PutPrice(ctx, bad, time.Now()); err == nil {
		t.Error("a cached rate above the input rate was accepted")
	}
}

// Two open-ended books for one model would make "which price applies" depend on
// read order. The partial unique index is what prevents it.
func TestOneOpenBookPerModel(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()

	// Written directly rather than through PutPrice, which closes the previous
	// book — the point is to prove the database refuses it too.
	_, err := db.pool.Exec(ctx, `
		INSERT INTO price_books (id, model, input_rate, output_rate, effective_from)
		VALUES ('a','m',1,1,now()), ('b','m',1,1,now())`)
	if err == nil {
		t.Error("two open-ended books for one model were accepted")
	}
}
