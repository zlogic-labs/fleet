package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The gateway prices a request the moment it settles, so a price refresh and a
// settlement run concurrently by construction. The fields they shared had no
// lock, which is a race over a Go map: not a stale read but a panic, on the one
// path a billing gateway cannot be allowed to crash on.
//
// Read this before trusting the test: it PASSES on the unlocked version unless
// the race detector is on. Verified both ways — without the lock it reports ok
// in 4.9s, and under `go test -race` it reports a dozen DATA RACE warnings. The
// database round trip inside Refresh dominates the nanosecond-wide assignment
// window, so timing alone will not reproduce it.
//
// So this is a `-race` test, not a `go test` one, and `make check` does not run
// it. Run `make test-race`. Anything that claims otherwise — including an
// earlier version of this comment — is telling you a test passed that could not
// have.

func TestConcurrentSettlementIsSafeAcrossARefresh(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books", "tenants", "projects")
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme')
	                 ON CONFLICT (id) DO NOTHING`)
	for i, model := range []string{"m1", "m2", "m3", "m4"} {
		mustExec(t, db, `INSERT INTO price_books (id, model, input_rate, output_rate, effective_from)
		                 VALUES ($1, $2, $3, $4, now())`,
			model+"-book", model, 1000+i, 2000+i)
	}

	store := NewPriceStore(db, time.Nanosecond) // always stale: every call reloads
	usage := openai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}

	var wg sync.WaitGroup
	const readers = 16
	const rounds = 40

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// Charge reads the snapshot; BookID reads the map the old code
				// swapped underneath it. Together they are the race.
				if _, err := store.Charge(ctx, "m1", "", usage); err != nil {
					t.Errorf("charge: %v", err)
					return
				}
				_ = store.BookID("m2", "")
			}
		}(i)
	}
	// A third of the goroutines reload instead of reading, so the write and the
	// reads genuinely overlap rather than happening to interleave by luck.
	for i := 0; i < readers/3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if err := store.Refresh(ctx); err != nil {
					t.Errorf("refresh: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Every reload must leave one coherent book behind. Reading a mix of a new
// pricer with an old map would still pass a race detector, so this asserts the
// property the snapshot exists to guarantee.
func TestARefreshLeavesThePricerAndTheIdsAgreeing(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "price_books")
	ctx := context.Background()
	for _, model := range []string{"m1", "m2"} {
		mustExec(t, db, `INSERT INTO price_books (id, model, input_rate, output_rate, effective_from)
		                 VALUES ($1, $2, 7, 9, now())`, model+"-book", model)
	}
	store := NewPriceStore(db, time.Hour)
	if err := store.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, model := range []string{"m1", "m2"} {
		if got := store.BookID(model, ""); got != model+"-book" {
			t.Errorf("BookID(%s) = %q, want %q", model, got, model+"-book")
		}
	}
	// A model with no book reads empty rather than panicking on a nil map.
	if got := store.BookID("absent", ""); got != "" {
		t.Errorf("BookID(absent) = %q, want empty", got)
	}
}
