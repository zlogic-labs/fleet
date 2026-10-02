package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
)

func TestConcurrentReservationsNeverExceedTheBudget(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 100, time.Hour)

	const workers, bound = 40, 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tryReserve(q, scope, usageFor(bound, 0)); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted > 10 {
		t.Errorf("granted %d reservations of %d against a budget of 100", granted, bound)
	}
	if got := used(t, q, scope, quota.TokensTotal); got > 100 {
		t.Errorf("committed = %d, more than the budget of 100", got)
	}
	if granted == 0 {
		t.Error("nothing was granted at all; the check is refusing unconditionally")
	}
}

// A scope with no rules is unlimited. An evaluation, a laptop, and every tenant
// nobody has thought about yet are all in this state and must serve traffic.
func TestNoRulesMeansUnlimited(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)

	for i := 0; i < 5; i++ {
		if _, err := tryReserve(q, scope, usageFor(1_000_000, 0)); err != nil {
			t.Fatalf("reserve %d: an unlimited scope refused: %v", i, err)
		}
	}
}

// A bucket no rule can read is dead weight on the write path of every request.
func TestPruneRemovesOnlyUnreachableBuckets(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 100, time.Hour)

	old := quota.BucketStart(time.Now().Add(-48*time.Hour), time.Minute)
	recent := quota.BucketStart(time.Now().Add(-2*time.Minute), time.Minute)
	mustExec(t, db, `INSERT INTO spend_counters (scope, bucket_start, tokens_total_spent)
		VALUES ($1,$2,1), ($1,$3,1)`, scope.Tenant, old, recent)

	n, err := q.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d buckets, want 1", n)
	}
	if used(t, q, scope, quota.TokensTotal) != 1 {
		t.Error("the reachable bucket was pruned; a check can now under-count")
	}
}
