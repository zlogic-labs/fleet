package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Price books as an operator sees them, which is a different question from
// pricing a request.
//
// The distinction these tests exist for: the fleet's own book for a model and a
// vendor's book for the same model are two prices, and one of them has to be
// able to move without touching the other. Everything below is that.

// declaredAt is when a test declares a book: a moment ago, not this instant.
//
// A book only becomes visible to ListBooks once the reader's clock has passed
// its effective instant, and the reader is PostgreSQL while the instant comes
// from this process. Nothing disciplines those two clocks here: measured at
// 192ms apart and still drifting, which is more than enough to hide a book
// declared in the current second — and the sign of the drift decides whether it
// is hidden, so the same test passes or fails on different runs.
//
// What these tests are about is which books exist and which provider owns
// them, not sub-second agreement between a laptop and a virtual machine, so they
// declare in the recent past. Calls a millisecond apart still land in the same
// second, which is what the collision cases need.
func declaredAt() time.Time { return time.Now().Add(-time.Second) }

// The id has to be derivable from the row, or an operator handed one cannot
// look the other up and a row cannot be traced back to the declaration that
// produced it.
//
// This is what makes the second-resolution truncation load-bearing rather than
// cosmetic: bookID is built at a second, so a row carrying a finer instant
// describes itself with a different id than the one it is filed under. It also
// pins the provider normalisation — the id must be the lower-cased one, not
// whichever spelling the operator typed.
func TestABooksIDCanBeRecomputedFromTheRowItself(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	// A sub-second instant on purpose: that is what a declaration actually
	// carries, and it is what the stored value has to normalise.
	declared := declaredAt().Add(437 * time.Millisecond)
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "gpt-4o", Provider: " OpenAI ", Rate: billing.Rate{Input: 2500, Output: 10000},
	}, declared); err != nil {
		t.Fatalf("put: %v", err)
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want 1", len(books))
	}
	if books[0].Provider != "openai" {
		t.Fatalf("provider is %q, want it normalised", books[0].Provider)
	}
	if n := books[0].EffectiveFrom.Nanosecond(); n != 0 {
		t.Errorf("stored instant carries %dns, finer than the id can name", n)
	}
	if want := bookID(books[0].Model, books[0].Provider, books[0].EffectiveFrom); books[0].ID != want {
		t.Errorf("row is filed under %q but describes itself as %q", books[0].ID, want)
	}
}

func TestListBooksReturnsOnlyTheOnesInForce(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	_, err := store.PutPrice(ctx, billing.Price{
		Model: "old", Rate: billing.Rate{Input: 1, Output: 2},
	}, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("put old: %v", err)
	}
	_, err = store.PutPrice(ctx, billing.Price{
		Model: "old", Rate: billing.Rate{Input: 3, Output: 4},
	}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("put current: %v", err)
	}
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "future", Rate: billing.Rate{Input: 5, Output: 6},
	}, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("put future: %v", err)
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]int64{}
	for _, b := range books {
		seen[b.Model] = b.Input
	}
	if len(books) != 1 {
		t.Fatalf("got %d books in force, want 1: %+v", len(books), books)
	}
	// A closed book listed beside its replacement would show an operator two
	// prices for one model with nothing to say which one a request paid.
	if seen["old"] != 3 {
		t.Errorf("the superseded book is the one in force: input %d, want 3", seen["old"])
	}
}

func TestRepricingAVendorLeavesTheFleetsOwnBookAlone(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	declare := func(model, provider string, input int64) {
		t.Helper()
		if _, err := store.PutPrice(ctx, billing.Price{
			Model: model, Provider: provider, Rate: billing.Rate{Input: input, Output: input * 2},
		}, declaredAt()); err != nil {
			t.Fatalf("put %s@%s: %v", model, provider, err)
		}
	}
	declare("qwen-7b", "", 1)
	declare("qwen-7b", "openai", 1500)

	declare("qwen-7b", "openai", 3000)

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byKey := map[billing.Provider]int64{}
	for _, b := range books {
		if b.Model != "qwen-7b" {
			t.Errorf("unexpected model %q", b.Model)
		}
		byKey[b.Provider] = b.Input
	}
	// Scoping the close to the provider is the whole point. Without it, a vendor
	// repricing would close the fleet's book too and every self-hosted request
	// for that model would drop to the floor rate.
	if byKey[""] != 1 {
		t.Errorf("the fleet's own book moved to %d when the vendor was repriced", byKey[""])
	}
	if byKey["openai"] != 3000 {
		t.Errorf("vendor book is %d, want 3000", byKey["openai"])
	}
}

func TestAVendorsCapitalisationIsNotASecondBook(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	for _, spelling := range []string{"OpenAI", " openai ", "OPENAI"} {
		if _, err := store.PutPrice(ctx, billing.Price{
			Model: "gpt-4o", Provider: spelling, Rate: billing.Rate{Input: 2500, Output: 10000},
		}, declaredAt()); err != nil {
			t.Fatalf("put %q: %v", spelling, err)
		}
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Two spellings of one vendor would split its spend across two report rows,
	// and the difference from the vendor's own invoice is exactly the amount that
	// split hid.
	if len(books) != 1 {
		t.Fatalf("got %d books for one vendor: %+v", len(books), books)
	}
	if books[0].Provider != "openai" {
		t.Errorf("provider stored as %q, want lowercased", books[0].Provider)
	}
}
