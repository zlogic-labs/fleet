package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// fixedClock lets the sliding window be driven instead of waited out.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(clock *fixedClock, p Policy) *Memory {
	m := NewMemory(func(string) Policy { return p })
	m.now = clock.now
	return m
}

// Settled usage has to leave the window like reserved usage does. A counter
// that only ever grows turns a per-minute token ceiling into a lifetime one:
// a tenant that spends its allowance steadily is refused forever, and the only
// remedy is an operator restarting the gateway.
func TestSettledUsageExpiresWithTheWindow(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0).Truncate(time.Minute)}
	m := newTestLimiter(clock, Policy{TokensPerMinute: 1000})

	// Spend 600 tokens, settle them, and let the window roll past.
	r, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 600})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	m.Settle(ctx, r, 600)

	clock.advance(2 * time.Minute)

	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 600}); err != nil {
		t.Errorf("two windows later the tenant is still refused: %v — settled usage never expires", err)
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
	m := newTestLimiter(clock, Policy{RequestsPerMinute: limit})

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
			go func() {
				defer wg.Done()
				start.Wait()
				if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err == nil {
					granted.Add(1)
				}
			}()
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
	m := newTestLimiter(clock, Policy{TokensPerMinute: 1000})

	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 600}); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	_, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 600})
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
// back, or a tenant that always asks for max_tokens=4096 and receives 40 tokens
// is charged 4096 for every request.
func TestSettleRefundsTheOverestimate(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policy{TokensPerMinute: 1000})

	// Three requests each reserving 400 tokens would exceed 1000 on the third,
	// even though each settled at only 40 actual tokens.
	for i := 0; i < 2; i++ {
		r, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 400})
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		m.Settle(ctx, r, 40)
	}
	// After refunds, the committed count is 80, so there is room for more.
	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 400}); err != nil {
		t.Fatalf("after settling 2x400 to 40 each, another 400 must fit: %v", err)
	}

	usage := usageFor(t, m, "acme")
	if usage.TokensSettled != 80 {
		t.Errorf("settled = %d, want 80", usage.TokensSettled)
	}
}

// An engine that reports more than was reserved must not push the counter
// negative, which would hand the tenant free allowance. The ledger still
// charges the real amount — only the limit counter refuses to go backwards.
func TestSettleNeverGoesNegative(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policy{TokensPerMinute: 1000})

	r, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 10})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	m.Settle(ctx, r, 900)

	u := usageFor(t, m, "acme")
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
	m := newTestLimiter(clock, Policy{RequestsPerMinute: 10})

	for i := 0; i < 10; i++ {
		if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err != nil {
			t.Fatalf("request %d inside the window: %v", i, err)
		}
	}
	// One second before the minute turns over, the limit is already reached.
	clock.advance(59 * time.Second)
	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err == nil {
		t.Error("a request at 59s must be refused while the limit is reached")
	}
	// Past the window the allowance is back.
	clock.advance(2 * time.Second)
	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err != nil {
		t.Errorf("a request after the window rolled must succeed: %v", err)
	}
}

func TestTenantsAreLimitedIndependently(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policy{RequestsPerMinute: 3})

	for i := 0; i < 3; i++ {
		if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err != nil {
			t.Fatalf("acme %d: %v", i, err)
		}
	}
	if _, err := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 1}); err == nil {
		t.Error("acme must be at its limit")
	}
	if _, err := m.Reserve(ctx, Request{Tenant: "other", Tokens: 1}); err != nil {
		t.Errorf("a second tenant must not inherit the first one's usage: %v", err)
	}
}

// An unlimited policy is the honest reading of "nothing is being charged", and
// it is what keeps a gateway with authentication disabled from being turned
// into a shared-bucket denial of service by one anonymous caller.
func TestUnlimitedPolicyAdmitsEverything(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policy{})

	for i := 0; i < 1000; i++ {
		if _, err := m.Reserve(ctx, Request{Tenant: "anyone", Tokens: 1_000_000}); err != nil {
			t.Fatalf("request %d refused under an unlimited policy: %v", i, err)
		}
	}
}

func TestInFlightIsTrackedAndReleased(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policy{RequestsPerMinute: 100})

	a, _ := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 10})
	b, _ := m.Reserve(ctx, Request{Tenant: "acme", Tokens: 10})
	if got := usageFor(t, m, "acme").InFlight; got != 2 {
		t.Errorf("in flight = %d, want 2", got)
	}
	m.Settle(ctx, a, 10)
	m.Settle(ctx, b, 10)
	if got := usageFor(t, m, "acme").InFlight; got != 0 {
		t.Errorf("in flight = %d, want 0 after both settled", got)
	}
}

func usageFor(t *testing.T, m *Memory, tenant string) Usage {
	t.Helper()
	for _, u := range m.Snapshot(context.Background()) {
		if u.Tenant == tenant {
			return u
		}
	}
	t.Fatalf("no usage reported for tenant %q", tenant)
	return Usage{}
}

func atoiInTest(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
