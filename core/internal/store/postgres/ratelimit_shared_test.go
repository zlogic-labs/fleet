package postgres

import (
	"context"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"sync"
	"sync/atomic"
	"testing"
)

// The limiter when more than one gateway is enforcing the same limit.
//
// The claim is not "a tenant is refused when it exceeds its rate" — it is
// that the refusal holds when several processes enforce it at once. A
// per-process counter passes every single-process test and fails this one,
// and fails it silently: each replica is individually correct, so nothing
// logs an error while the tenant spends several times what they bought.
//
// Ten times as many callers as the limit allows. An earlier version ran 40
// against a limit of 50, which passes even if the lock is removed entirely —
// the test could not fail, so it proved nothing.
func TestTwoGatewaysShareTheLimit(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	const limit = 20
	a, b := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: limit})

	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lim := a
			if i%2 == 1 {
				lim = b
			}
			if _, err := lim.Reserve(context.Background(),
				ratelimit.Request{Scope: ratelimit.Tenant("acme"), Tokens: 1}); err == nil {
				granted.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := granted.Load(); int(got) > limit {
		t.Fatalf("granted %d against a limit of %d across two gateways", got, limit)
	}
	if got := granted.Load(); got == 0 {
		t.Fatal("nothing was granted at all")
	}
}

// Settling replaces the bound with the real cost. Refunding only the
// difference leaves the original deduction in place whenever the engine
// reported more than was reserved, charging the tenant twice over.
func TestSettlingReplacesTheBoundWithTheRealCost(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: 100, TokensPerMinute: 100_000})
	scope := ratelimit.Tenant("acme")

	r := mustReserveRate(t, l, scope, 5000)
	if got := rateUsed(t, db, "acme", "tokens"); got != 5000 {
		t.Errorf("committed = %d after reserving, want 5000", got)
	}
	l.Settle(context.Background(), r, 7)
	if got := rateUsed(t, db, "acme", "tokens"); got != 7 {
		t.Errorf("committed = %d after settling 7, want 7 — the reservation was not released", got)
	}
}

// A request that never reached an engine is not a request. Charging it anyway
// lets a caller burn a minute's allowance on a misspelled model name.
func TestSettlingToZeroRefundsTheRequestCount(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: 100, TokensPerMinute: 100_000})
	scope := ratelimit.Tenant("acme")

	r := mustReserveRate(t, l, scope, 500)
	l.Settle(context.Background(), r, 0)
	if got := rateUsed(t, db, "acme", "requests"); got != 0 {
		t.Errorf("requests = %d after a request that generated nothing, want 0", got)
	}
	if got := rateUsed(t, db, "acme", "tokens"); got != 0 {
		t.Errorf("tokens = %d after a request that generated nothing, want 0", got)
	}
}

// The ceiling reads settled usage as well as reservations. Settling moves the
// tokens out of the reservation column, so a check reading only that column
// sees the window drain after every request completes — and a steady stream of
// short requests never reaches its ceiling at all.
func TestTheTokenCeilingCountsSettledUsage(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{TokensPerMinute: 2000})
	scope := ratelimit.Tenant("acme")

	for i := 0; i < 4; i++ {
		r := mustReserveRate(t, l, scope, 100)
		l.Settle(context.Background(), r, 500)
	}
	_, err := l.Reserve(context.Background(), ratelimit.Request{Scope: scope, Tokens: 1})
	if err == nil {
		t.Fatalf("four settled requests of 500 were admitted against a 2000 ceiling")
	}
}

// A project partition refusing must not spend the tenant envelope. Otherwise a
// tenant is charged for the request volume of the project it cannot control.
func TestAPartitionRefusalDoesNotSpendTheEnvelope(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme') ON CONFLICT DO NOTHING`)
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name) VALUES ('acme/research','acme','research')
		ON CONFLICT DO NOTHING`)
	// The envelope is generous; the partition is not. The whole point is that
	// the tight level is the one that refuses.
	l := NewRateLimiter(db, func(ratelimit.Scope) ratelimit.Policies {
		return ratelimit.Policies{
			Envelope:  ratelimit.Policy{RequestsPerMinute: 1000},
			Partition: ratelimit.Policy{RequestsPerMinute: 2},
		}
	})
	scope := ratelimit.Project("acme", "research")

	mustReserveRate(t, l, scope, 10)
	mustReserveRate(t, l, scope, 10)
	if _, err := l.Reserve(context.Background(), ratelimit.Request{Scope: scope, Tokens: 10}); err == nil {
		t.Fatal("a third request passed a partition limit of 2")
	}
	if got := rateUsed(t, db, "acme", "requests"); got != 2 {
		t.Errorf("envelope counted %d requests, want 2 — the refused one was charged", got)
	}
}

// The envelope and the partition are separate counters, not the tighter of the
// two. Merging them makes a tenant with ten projects under a 300 rpm envelope
// capable of 3000 rpm.
func TestTheEnvelopeAndPartitionAreCountedSeparately(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: 50})

	for i := 0; i < 4; i++ {
		mustReserveRate(t, l, ratelimit.Project("acme", "research"), 10)
	}
	if got := rateUsed(t, db, "acme", "requests"); got != 4 {
		t.Errorf("envelope = %d, want 4", got)
	}
	if got := rateUsed(t, db, "acme/research", "requests"); got != 4 {
		t.Errorf("partition = %d, want 4", got)
	}
}

// Spending outside the window must stop counting, or a per-minute ceiling
// becomes a lifetime one that only a restart clears.
//
// Asserted through Snapshot rather than by reading the table, because the rows
// are still there — that is the second half of this test.
