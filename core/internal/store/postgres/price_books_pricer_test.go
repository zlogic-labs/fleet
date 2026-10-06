package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The pricer and the listing must agree about what is in force, or an operator
// reads a price and a tenant is charged a different one.
func TestTheBookAndThePricerSeeTheSamePrices(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Hour)

	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "m", Provider: "v", Rate: billing.Rate{Input: 7, Output: 9},
	}, declaredAt()); err != nil {
		t.Fatalf("put: %v", err)
	}
	pricer, err := store.Pricer(ctx)
	if err != nil {
		t.Fatalf("pricer: %v", err)
	}
	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want 1", len(books))
	}
	// Compared through a charge rather than by reading the book back: the pair
	// disagreeing is not what a tenant experiences, a tenant experiencing a
	// different amount than the page quoted is.
	usage := openai.Usage{PromptTokens: int(billing.UnitsPer / 100), CompletionTokens: int(billing.UnitsPer / 10)}
	amount, err := pricer.Charge("m", "v", usage)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	book := books[0]
	want := billing.Amount(int64(usage.PromptTokens)*book.Input + int64(usage.CompletionTokens)*book.Output)
	if amount != want {
		t.Errorf("charged %d against a listed book of %+v, want %d", amount, book.Rate, want)
	}
}
