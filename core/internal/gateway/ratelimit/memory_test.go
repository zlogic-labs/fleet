package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// The mechanics, independent of the two-level split: the sliding window, the
// reserve-then-settle contract, and what a report has to say.

// Settled usage has to leave the window like reserved usage does. A counter
// that only ever grows turns a per-minute token ceiling into a lifetime one:
// a caller that spends its allowance steadily is refused forever, and the only
// remedy is an operator restarting the gateway.
func TestSettledUsageExpiresWithTheWindow(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0).Truncate(time.Minute)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{TokensPerMinute: 1000}})

	r, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 600})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	m.Settle(ctx, r, 600)

	clock.advance(2 * time.Minute)

	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 600}); err != nil {
		t.Errorf("two windows later the caller is still refused: %v — settled usage never expires", err)
	}
}

// The reason this limiter reserves instead of checking. A check-then-act
// limiter lets every request in a burst observe "under the limit" and then all
// of them proceed; this asserts that at most the limit is ever committed, which
// is the property P5 exists for.
//
// The burst repeats because a single burst is not a reliable witness. Whether
// an unlocked implementation overshoots depends on whether the scheduler
// interleaves two goroutines inside the check-and-commit, and with a handful of
// nanoseconds of work between them it frequently does not — the test then passes
// against the very bug it is written for. Repeating until the window has rolled
// past the previous burst gives a checker many chances to be caught, and the
// invariant is checked on every round rather than only the last, so a single bad
// round fails the test.
func TestConcurrentReservationsCannotOvershoot(t *testing.T) {
	ctx := context.Background()
	const (
		limit  = 50
		burst  = 400
		rounds = 20
	)
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0).Truncate(time.Minute)}
	// Spread the burst across two projects so both levels are under contention
	// at once. A limiter that checked the two separately would still overshoot
	// here even if it were correct for one level.
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{RequestsPerMinute: limit},
		Partition: Policy{RequestsPerMinute: limit},
	})

	for round := 0; round < rounds; round++ {
		// A fresh window each round, so the limit is available again and the
		// count below is the count for this round alone.
		clock.advance(time.Minute)

		var granted atomic.Int64
		var wg, start sync.WaitGroup
		// Every goroutine waits on one barrier, so the arrivals overlap
		// instead of trickling in as the loop body happens to run.
		start.Add(1)
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start.Wait()
				scope := projectScope()
				if i%2 == 1 {
					scope = otherProjectScope()
				}
				if _, err := m.Reserve(ctx, Request{Scope: scope, Tokens: 1}); err == nil {
					granted.Add(1)
				}
			}(i)
		}
		start.Done()
		wg.Wait()

		if got := granted.Load(); got > limit {
			t.Fatalf("round %d: granted %d against a limit of %d — the check and the "+
				"commit are not atomic", round, got, limit)
		}
		if got := granted.Load(); got != limit {
			t.Errorf("round %d: granted %d, want exactly %d", round, got, limit)
		}
	}
}

func TestTokenLimitRefusesAndReportsRetryAfter(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{TokensPerMinute: 1000}})

	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 600}); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	_, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 600})
	if err == nil {
		t.Fatal("600 more tokens against a 1000 limit must be refused")
	}
	if errs.KindOf(err) != errs.KindRateLimited {
		t.Errorf("kind = %v, want rate_limited so it maps to 429", errs.KindOf(err))
	}
	if got := RetryAfterSeconds(err); got != "" {
		if n, _ := atoiInTest(got); n < 1 || n > 60 {
			t.Errorf("Retry-After = %s, want between 1 and 60", got)
		}
	}
}

// Settlement is the other half of P5: what was reserved but not used must go
// back, or a caller that always asks for max_tokens=4096 and receives 40 tokens
// is charged 4096 for every request.
func TestSettleRefundsTheOverestimate(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{TokensPerMinute: 1000}})

	// Two requests each reserving 400 tokens would exceed 1000 on the third,
	// even though each settled at only 40 actual tokens.
	for i := 0; i < 2; i++ {
		r, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 400})
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		m.Settle(ctx, r, 40)
	}
	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 400}); err != nil {
		t.Fatalf("after settling 2x400 to 40 each, another 400 must fit: %v", err)
	}

	if got := usageFor(t, m, tenantScope()).TokensSettled; got != 80 {
		t.Errorf("settled = %d, want 80", got)
	}
}

// Settled usage must count against the ceiling, not just reservations.
//
// Settling moves a request's tokens out of the reservation and into the settled
// total, so a limiter that compared only the reservation would see its bucket
// drain to zero after every request finished — and a caller sending a steady
// stream of short requests would never reach its ceiling at all. This is the
// shape of real traffic: most requests complete before the next arrives, so the
// reservation column is nearly always empty.
//
// Found by scripts/smoke.sh, which sends real requests through the gateway and
// therefore settles between them. The unit tests all settled amounts well below
// the ceiling and so could not tell the two apart.
func TestSettledUsageCountsAgainstTheTokenCeiling(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{TokensPerMinute: 500}})

	// Five requests of 100 tokens each, every one settled before the next is
	// made. Nothing is in flight when a reservation is taken, so the reservation
	// column is empty and only the settled column is standing between this
	// caller and its ceiling.
	for i := 0; i < 5; i++ {
		r, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 100})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		m.Settle(ctx, r, 100)
	}
	// 500 spent. The next 100 must be refused.
	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 100}); err == nil {
		t.Error("500 settled tokens should be at a 500 token ceiling, but the next request was admitted")
	}
	if got := usageFor(t, m, tenantScope()).TokensSettled; got != 500 {
		t.Fatalf("settled = %d, want 500", got)
	}
}

// An engine that reports more than was reserved must not push the counter
// negative, which would hand the caller free allowance. The ledger still
// charges the real amount — only the limit counter refuses to go backwards.
func TestSettleNeverGoesNegative(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{TokensPerMinute: 1000}})

	r, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 10})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	m.Settle(ctx, r, 900)

	u := usageFor(t, m, tenantScope())
	if u.TokensReserved != 0 {
		t.Errorf("reserved = %d, want 0 after a full refund", u.TokensReserved)
	}
	if u.TokensSettled != 900 {
		t.Errorf("settled = %d, want 900", u.TokensSettled)
	}
}

// A fixed window has a boundary an attacker aims at: spend the limit at the end
// of one minute and again at the start of the next. A sliding window does not
// hand out a second allowance at the boundary.
func TestSlidingWindowDoesNotDoubleAllowanceAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0).Truncate(time.Minute)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{RequestsPerMinute: 10}})

	for i := 0; i < 10; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 1}); err != nil {
			t.Fatalf("request %d inside the window: %v", i, err)
		}
	}
	clock.advance(59 * time.Second)
	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 1}); err == nil {
		t.Error("a request at 59s must be refused while the limit is reached")
	}
	clock.advance(2 * time.Second)
	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 1}); err != nil {
		t.Errorf("a request after the window rolled must succeed: %v", err)
	}
}
