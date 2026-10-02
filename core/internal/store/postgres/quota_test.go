package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The budget, against a real database.
//
// The claim under test is not "a tenant is refused when it overspends" — it is
// "a tenant is never *allowed* to overspend". A check-then-act implementation
// passes the first test and fails the rest, and fails the rest only under
// concurrency, which is exactly when a real customer is present.

func TestSpendIsReservedThenSettled(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 100, 100)

	// The reservation is visible immediately, at its upper bound, before the
	// request has run.
	r := mustReserve(t, q, scope, w, 60)
	if got, want := mustSpent(t, q, scope, w), u(60); got != want {
		t.Errorf("committed = %s after reserving, want %s", got, want)
	}

	q.Settle(context.Background(), r, u(7))
	if got, want := mustSpent(t, q, scope, w), u(7); got != want {
		t.Errorf("committed = %s after settling 7, want 7 — the reservation was not released", got)
	}
}

// The reservation is an upper bound, not the bill. Settling for less must give
// the difference back, or a tenant is charged the worst case of every request
// it ever made.
func TestSettlingBelowTheBoundRefundsTheDifference(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 1000, 1000)

	for i := 0; i < 5; i++ {
		q.Settle(context.Background(), mustReserve(t, q, scope, w, 100), u(3))
	}
	if got, want := mustSpent(t, q, scope, w), u(15); got != want {
		t.Errorf("committed = %s after five requests of 3, want %s — the bounds were not refunded", got, want)
	}
}

// A budget that cannot cover the bound refuses it whole. A partial commit would
// be worse than useless: the request is refused anyway, having moved the
// counter first.
func TestABoundOverTheBudgetIsRefusedWhole(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 100, 100)

	if _, err := tryReserve(q, scope, w, 101); quota.AsExceeded(err) == nil {
		t.Fatalf("a bound of 101 against a budget of 100 was admitted: %v", err)
	}
	if got := mustSpent(t, q, scope, w); got != 0 {
		t.Errorf("committed = %s after a refusal, want 0 — a refused reservation left a mark", got)
	}
}

// A request that really did cost more than it reserved is recorded in full. The
// budget has to agree with the ledger, or reconciliation finds a bill nobody
// was charged.
func TestAChargeAboveTheBoundIsStillRecorded(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 1000, 1000)

	q.Settle(context.Background(), mustReserve(t, q, scope, w, 10), u(250))
	if got, want := mustSpent(t, q, scope, w), u(250); got != want {
		t.Errorf("committed = %s, want 250 — a charge above the bound was truncated", got)
	}
}

// The tenant envelope is not the sum of its projects' budgets. This is the same
// multiplication trap the rate limiter has, and here it costs money: a tenant
// with ten projects would have ten times the allowance if the levels were added.
func TestProjectsPartitionTheEnvelopeRatherThanAddingToIt(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 100, 100)

	r := mustReserve(t, q, scope, w, 100)
	q.Settle(context.Background(), r, u(100))

	// A second project of the same tenant may not spend anything more: the
	// envelope is gone. If the two levels were merged this would succeed.
	second := ratelimit.Scope{Tenant: scope.Tenant, Project: "second"}
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name, budget_units)
		VALUES ('acme/second','acme','second',100)`)
	if _, err := tryReserve(q, second, w, 1); quota.AsExceeded(err) == nil {
		t.Fatal("a second project spent after the envelope was exhausted; the levels were added")
	}
}

// A project that is out of budget must not spend the tenant's envelope. The
// partition is a slice of the envelope, and charging the envelope for traffic
// the partition refused is the tenant paying for a limit it set itself.
func TestARefusedPartitionDoesNotSpendTheEnvelope(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 100, 20)

	// One request fits the partition's 20.
	q.Settle(context.Background(), mustReserve(t, q, scope, w, 15), u(15))

	// Three more are refused by the partition. If the rollback were missing,
	// the envelope would show 15 + 45.
	for i := 0; i < 3; i++ {
		if _, err := tryReserve(q, scope, w, 15); quota.AsExceeded(err) == nil {
			t.Fatalf("request %d: a project over its own 20 was admitted", i+2)
		}
	}

	tenant := ratelimit.Scope{Tenant: scope.Tenant}
	if got, want := mustSpent(t, q, tenant, w), u(15); got != want {
		t.Errorf("tenant envelope committed = %s, want %s — refused traffic was charged to it",
			got, want)
	}
}

// Zero means unlimited, and a deployment with no budgets set is the common case.
// It has to serve traffic.
func TestZeroBudgetMeansUnlimited(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 0, 0)

	for i := 0; i < 5; i++ {
		if _, err := tryReserve(q, scope, w, 1_000_000); err != nil {
			t.Fatalf("reserve %d: an unlimited scope refused: %v", i, err)
		}
	}
}

// Concurrency is the only reason reservation exists. Many requests racing for
// the last of a budget must not collectively pass it.
func TestConcurrentReservationsNeverExceedTheBudget(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope, w := budgetFixture(t, db, 100, 0)

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
			if _, err := tryReserve(q, scope, w, bound); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted > 100/bound {
		t.Errorf("granted %d reservations of %d against a budget of 100", granted, bound)
	}
	if got := mustSpent(t, q, scope, w); got > u(100) {
		t.Errorf("committed = %s, more than the budget of 100", got)
	}
	if granted == 0 {
		t.Error("nothing was granted at all; the check is refusing unconditionally")
	}
}

// ── fixtures ────────────────────────────────────────────────

// budgetFixture returns a quota over acme/research with the given budgets in
// whole units, the scope, and a window inside the current month.
//
// The scope carries the project's bare name while the row's id is the
// tenant/name path, which is the same shape a principal has at runtime — the
// mismatch is invisible until a query uses the wrong one.
func budgetFixture(t *testing.T, db *DB, envelope, partition int64) (*Quota, ratelimit.Scope, quota.Window) {
	t.Helper()
	mustExec(t, db, `INSERT INTO tenants (id, name, budget_units)
		VALUES ('acme','Acme',$1)
		ON CONFLICT (id) DO UPDATE SET budget_units = EXCLUDED.budget_units`, envelope)
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name, budget_units)
		VALUES ('acme/research','acme','research',$1)
		ON CONFLICT (id) DO UPDATE SET budget_units = EXCLUDED.budget_units`, partition)
	return NewQuota(db),
		ratelimit.Scope{Tenant: "acme", Project: "research"},
		quota.CalendarMonth(time.Now())
}

func truncateSpend(t *testing.T, db *DB) {
	t.Helper()
	truncate(t, db, "spend_counters", "api_keys", "projects", "tenants")
}

// u converts whole units to the micro-unit an Amount is expressed in.
func u(n int64) billing.Amount { return billing.Amount(n) * billing.MicroPerUnit }

func tryReserve(q *Quota, scope ratelimit.Scope, w quota.Window, bound int64) (quota.Reservation, error) {
	return q.Reserve(context.Background(), quota.Request{
		Scope: scope, Bound: u(bound), Now: w.From.Add(time.Hour),
	})
}

func mustReserve(t *testing.T, q *Quota, scope ratelimit.Scope, w quota.Window, bound int64) quota.Reservation {
	t.Helper()
	r, err := tryReserve(q, scope, w, bound)
	if err != nil {
		t.Fatalf("reserve %d: %v", bound, err)
	}
	return r
}

func mustSpent(t *testing.T, q *Quota, scope ratelimit.Scope, w quota.Window) billing.Amount {
	t.Helper()
	got, err := q.Spent(context.Background(), scope, w)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	return got
}
