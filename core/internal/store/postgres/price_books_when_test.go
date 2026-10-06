package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// What happens when a book is declared twice, or declared for a period that is
// already priced.
//
// Both are things an operator does by accident and both used to arrive as a
// unique-constraint violation, which reads as a fault rather than as a question
// about the request. The id is derived from the effective date to the second,
// so "declared twice" is not a corner case — two adjacent calls from a script
// land in the same second routinely.

// Reading which book is in force has to be per provider, or a vendor's book is
// judged against the fleet's. The fleet's book for a model routinely starts
// later than a vendor's does — an operator adds a fallback route months after
// the model was priced locally — and an unscoped read would see that later
// start, decide the requested period was already priced, and refuse the
// declaration. The vendor's book is the one thing a vendor upstream cannot
// work without.
func TestAVendorBookIsJudgedAgainstTheVendorsOwnBookNotTheFleets(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	// The fleet priced the model first...
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "qwen-7b", Rate: billing.Rate{Input: 1, Output: 2},
	}, declaredAt()); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	// ...and the vendor's route is declared retroactively, from before the
	// fleet's book started.
	vendor := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := store.PutPrice(ctx, billing.Price{
		Model: "qwen-7b", Provider: "openai", Rate: billing.Rate{Input: 1500, Output: 6000},
	}, vendor); err != nil {
		t.Fatalf("vendor, declared retroactively: %v", err)
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("got %d books, want the vendor's to have been added beside the fleet's: %+v", len(books), books)
	}
}

// Two declarations at once. Neither can see the other's open book, so one of
// them hits the primary key — and the answer has to be a conflict the caller
// can act on, not an internal error, because from where the operator sits the
// two are indistinguishable from a retry arriving twice.
func TestConcurrentDeclarationsConflictRatherThanFail(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	store := NewPriceStore(db, time.Minute)

	at := declaredAt()
	const writers = 4
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			_, err := store.PutPrice(context.Background(), billing.Price{
				Model: "gpt-4o", Provider: "openai",
				Rate: billing.Rate{Input: int64(1000 + i), Output: 4000},
			}, at)
			results <- err
		}()
	}
	var conflicts, other int
	for range writers {
		if err := <-results; err != nil {
			if Conflict(err) {
				conflicts++
			} else {
				other++
				t.Errorf("loser reported %v, want a conflict", err)
			}
		}
	}
	if other > 0 {
		return
	}
	if conflicts == 0 {
		t.Skip("the writers serialised; nothing collided, so this proves nothing here")
	}
	books, err := store.ListBooks(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want exactly one survivor: %+v", len(books), books)
	}
}

// Two declarations for the same book within one second, which is what a retry
// or a backfilled revision looks like. The id is derived from the effective
// date to the second, so without a deliberate answer this is an error on an
// operator action that means something.
func TestRedeclaringTheSameInstantReplacesRatherThanFails(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	at := declaredAt().Truncate(time.Second)
	for _, input := range []int64{2500, 3000} {
		if _, err := store.PutPrice(ctx, billing.Price{
			Model: "gpt-4o", Provider: "openai", Rate: billing.Rate{Input: input, Output: input * 4},
		}, at); err != nil {
			t.Fatalf("put %d at the same instant: %v", input, err)
		}
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want the last declaration to have replaced the first: %+v", len(books), books)
	}
	if books[0].Input != 3000 {
		t.Errorf("book is %d, want the later declaration (3000)", books[0].Input)
	}
}

// The same second, but not the same instant — which is what two adjacent calls
// actually produce. The collision has to be decided at the resolution the id is
// built at, or closing the first book at the second one's instant writes an
// interval that ends before it begins and the schema rejects it.
func TestRedeclaringWithinTheSameSecondReplacesRatherThanFails(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	at := declaredAt().Truncate(time.Second)
	for i, input := range []int64{2500, 3000} {
		when := at.Add(time.Duration(i) * 300 * time.Millisecond)
		if _, err := store.PutPrice(ctx, billing.Price{
			Model: "gpt-4o", Provider: "openai", Rate: billing.Rate{Input: input, Output: input * 4},
		}, when); err != nil {
			t.Fatalf("put %d at %s: %v", input, when.Format(time.RFC3339Nano), err)
		}
	}

	books, err := store.ListBooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(books) != 1 || books[0].Input != 3000 {
		t.Fatalf("got %+v, want one book at 3000", books)
	}
}

// Backdating onto a period that is already priced. The book in force starts
// later than the instant asked for, so there is no interval to hand over, and
// the only way to honour the request would be to rewrite a period that has
// already been charged against. That has to arrive as a conflict naming what is
// in force — not as a constraint violation, and not as a silent replacement.
func TestBackdatingOntoAPricedPeriodIsAConflictThatSaysWhatIsInForce(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	store := NewPriceStore(db, time.Minute)

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	inForce := base.Add(time.Hour)
	put := func(at time.Time, input int64) error {
		_, err := store.PutPrice(ctx, billing.Price{
			Model: "gpt-4o", Provider: "openai", Rate: billing.Rate{Input: input, Output: input * 4},
		}, at)
		return err
	}
	if err := put(base, 2500); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := put(inForce, 3000); err != nil {
		t.Fatalf("second: %v", err)
	}

	err := put(base, 2750)
	if !Conflict(err) {
		t.Fatalf("backdating onto a priced period: %v, want a conflict", err)
	}
	// An operator who gets this needs to know which instant to declare from,
	// and the refusal is useless without it.
	if !strings.Contains(err.Error(), inForce.UTC().Format(time.RFC3339)) {
		t.Errorf("refusal does not say when the book in force starts: %v", err)
	}
	books, err2 := store.ListBooks(ctx)
	if err2 != nil {
		t.Fatalf("list: %v", err2)
	}
	if len(books) != 1 || books[0].Input != 3000 {
		t.Fatalf("the refused write disturbed what was priced: %+v", books)
	}
}
