package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Repricing a vendor must not close the fleet's own book for the same model.
//
// The two live in one table and are told apart by the provider column, so every
// write that supersedes a book has to be scoped by it. Reading the open book
// scoped is not enough: the close that follows is a separate statement, and
// leaving the provider out of it ends every open book for the model at once.
//
// The shape is ordinary. A vendor route is added, works for a while, and is
// repriced when the vendor changes its list. If that second declaration also
// ends the fleet's own book, every local request for the model drops to the
// floor rate from that instant — silently, with no error anywhere, and with a
// ledger that keeps charging tokens against a rate nobody chose.
func TestRepricingAVendorLeavesTheFleetsOwnBookInForce(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	now := time.Now().Truncate(time.Second)
	fleetFrom := now.Add(-3 * 24 * time.Hour)
	vendorFrom := now.Add(-2 * 24 * time.Hour)
	repriced := now.Add(-24 * time.Hour)

	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "qwen-7b", Rate: billing.Rate{Input: 1, Output: 2},
	}, fleetFrom); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "qwen-7b", Provider: "openai", Rate: billing.Rate{Input: 1500, Output: 6000},
	}, vendorFrom); err != nil {
		t.Fatalf("vendor: %v", err)
	}
	// The repricing is the ordinary path: a new book starting after the open
	// one, which closes it.
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "qwen-7b", Provider: "openai", Rate: billing.Rate{Input: 1600, Output: 6400},
	}, repriced); err != nil {
		t.Fatalf("vendor repriced: %v", err)
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("got %d books in force, want the fleet's and the vendor's: %+v", len(books), books)
	}
	for _, b := range books {
		switch b.Provider {
		case "":
			if b.Input != 1 {
				t.Errorf("the fleet's book now charges %d per MTok, want 1", b.Input)
			}
		case "openai":
			if b.Input != 1600 {
				t.Errorf("the vendor's book charges %d, want the repriced 1600", b.Input)
			}
		default:
			t.Errorf("unexpected provider %q in %+v", b.Provider, b)
		}
	}
}
