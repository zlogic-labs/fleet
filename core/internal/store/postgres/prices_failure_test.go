package postgres

import (
	"context"
	"testing"
	"time"
)

// A failed refresh must not lose the books already held, and must not wedge
// the store either.
//
// The second half is the one worth writing down. An earlier version of Pricer
// took a read lock with defer inside the failure branch and then took another
// one on the way out. sync.RWMutex readers do not queue behind other readers,
// so the second RLock succeeds and only one RUnlock ever runs: every writer
// after that point blocks forever, and the gateway stops pricing. It needs
// contention to show up, which is why it is exercised rather than asserted by
// inspection.
func TestReadingThePriceStoreDoesNotBlockOnAFailedRefresh(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO price_books (id, model, input_rate, output_rate, effective_from)
	                 VALUES ('m1-book', 'm1', 5, 9, now())`)

	store := NewPriceStore(db, time.Hour)
	if err := store.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if id := store.BookID("m1"); id != "m1-book" {
		t.Fatalf("premise: BookID(m1) = %q, want m1-book", id)
	}

	// A cancelled context makes every refresh fail for an ordinary reason.
	dead, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := store.Pricer(dead); err != nil {
		t.Fatalf("a failed refresh with books already held must not be fatal, got %v", err)
	}
	if id := store.BookID("m1"); id != "m1-book" {
		t.Errorf("BookID(m1) = %q after a failed refresh, want m1-book", id)
	}

	// And the writers must still get through afterwards, which is what a
	// leaked read lock would prevent.
	done := make(chan error, 1)
	go func() { done <- store.Refresh(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("refresh after a failed one: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("refresh blocked after a failure; a read lock was leaked")
	}
}

// Nothing may be published by a refresh that did not complete, or the next
// reader trusts a book that was never read.
func TestAFailedRefreshPublishesNothing(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	store := NewPriceStore(db, time.Hour)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Refresh(dead); err == nil {
		t.Fatal("Refresh against a dead context reported success")
	}
	if id := store.BookID("m1"); id != "" {
		t.Errorf("a failed refresh published BookID(m1) = %q", id)
	}
}

// "No database configured" reaches the store as a DB with no pool. PriceStore
// reports that rather than dereferencing it, because Pricer is on the settlement
// path and a panic there takes the gateway down instead of failing a request.
//
// Scoped to PriceStore deliberately. About twenty methods in this package
// dereference db.pool the same way, and none of them is reachable in that
// state through the shipped binaries -- the stores are only ever constructed
// when Open succeeded. Guarding each one would be twenty repetitions of a
// check for a state the code cannot be put into; this one is guarded because
// it is the hottest path and the test found it there.
func TestAnUnopenedDatabaseSaysSoInsteadOfPanicking(t *testing.T) {
	store := NewPriceStore(&DB{}, time.Hour)
	if _, err := store.Pricer(context.Background()); err == nil {
		t.Error("Pricer on an unopened database reported success")
	}
	if err := store.Refresh(context.Background()); err == nil {
		t.Error("Refresh on an unopened database reported success")
	}
	if id := store.BookID("m1"); id != "" {
		t.Errorf("BookID on an unopened database = %q, want empty", id)
	}
}
