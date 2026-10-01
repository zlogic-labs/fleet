package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Memory is an in-process Limiter using a sliding window.
//
// A sliding window rather than a fixed one because a fixed window has a
// boundary an attacker aims at: spend the whole limit in the last second of
// minute N and the whole limit again in the first second of minute N+1, which
// is twice the limit in two seconds. Buckets smooth that, and the counters are
// small enough that the bookkeeping is free.
//
// Correct under concurrency because Reserve takes the tenant's lock once and
// decides everything inside it. A separate lock per tenant rather than one
// global lock, so a tenant with a thousand in-flight streams does not serialise
// the other tenants behind it.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]*counter

	// PolicyFor supplies the default limits for a tenant, so a limit can be
	// changed in configuration without restarting every in-flight request's
	// accounting. Returning an unlimited policy disables the limiter.
	PolicyFor func(tenant string) Policy

	// Window is the period the limit applies over.
	Window time.Duration

	// now is a field so a test can drive time instead of sleeping. A limiter
	// whose reset behaviour can only be observed by waiting a minute is a
	// limiter whose reset behaviour goes untested.
	now func() time.Time
	seq atomic.Uint64
}

// DefaultWindow is the period RequestsPerMinute and TokensPerMinute apply
// over.
const DefaultWindow = time.Minute

func NewMemory(policyFor func(string) Policy) *Memory {
	m := &Memory{
		tenants:   map[string]*counter{},
		Window:    DefaultWindow,
		now:       time.Now,
		PolicyFor: policyFor,
	}
	if policyFor == nil {
		m.PolicyFor = func(string) Policy { return Policy{} }
	}
	return m
}

var _ Limiter = (*Memory)(nil)

// Reserve commits the deduction for one request.
func (m *Memory) Reserve(_ context.Context, req Request) (Reservation, error) {
	policy := m.policyFor(req.Tenant)
	if policy.Unlimited() {
		return Reservation{tenant: req.Tenant, policy: policy}, nil
	}

	now := m.now()
	reset := m.windowEnd(now)

	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.counterFor(req.Tenant, policy)
	requests, tokens, settled := c.totals(now, m.Window)

	if policy.RequestsPerMinute > 0 && requests >= policy.RequestsPerMinute {
		return Reservation{}, limited("request rate", requests, policy.RequestsPerMinute, reset)
	}
	// The token check is against reserved-plus-already-settled, so a tenant
	// cannot spend the same minute's tokens twice by having many requests in
	// flight at once. Both figures are read from the ring, so both leave when
	// the window rolls over.
	if policy.TokensPerMinute > 0 {
		committed := tokens + settled
		if committed+req.Tokens > policy.TokensPerMinute {
			return Reservation{}, limited("token rate", committed, policy.TokensPerMinute, reset)
		}
	}

	slot := c.slot(now)
	slot.requests++
	slot.tokens += req.Tokens
	c.inFlight++

	return Reservation{
		id:       fmt.Sprintf("rsv-%d-%s", m.seq.Add(1), req.Tenant),
		tenant:   req.Tenant,
		policy:   policy,
		Reserved: req.Tokens,
		counted:  true,
	}, nil
}

// Settle converts a reservation into settled usage.
//
// The whole reservation is removed from the buckets and the actual amount is
// recorded instead, rather than "refunding the difference". Those two are the
// same arithmetic when actual <= reserved and not the same when it is greater:
// refunding a negative difference leaves the original deduction in place *and*
// adds the actual, so a tenant whose engine reported more than reserved would
// be charged for both. Removing all of it and recording actual is correct in
// both directions.
func (m *Memory) Settle(_ context.Context, r Reservation, actual int) {
	if !r.counted && actual <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.tenants[r.tenant]
	if !ok {
		return
	}
	if c.inFlight > 0 {
		c.inFlight--
	}
	// Settling to zero means the request never reached an engine — a routing
	// miss, or a replica that refused. It gives back both dimensions, not just
	// the tokens: a request that consumed no GPU second is not a request, and
	// charging it against the per-minute request count would let a tenant burn
	// a minute's allowance on a mistyped model name.
	if actual <= 0 {
		if r.counted {
			c.release(r.Reserved, 1, m.now())
		}
		return
	}
	if r.counted && r.Reserved > 0 {
		// Oldest first: the deduction was recorded when the request arrived,
		// and that is the bucket furthest along towards falling out of the
		// window, so removing it there expires the amount soonest.
		c.release(r.Reserved, 0, m.now())
	}
	// The actual usage goes into the ring rather than a scalar, so it expires
	// with the window like everything else. A scalar would make a per-minute
	// ceiling into a lifetime one: the tenant would be refused forever after
	// spending its allowance once, and only a restart would clear it.
	//
	// It is recorded against the current second, not the second the request
	// arrived, because a long generation spans seconds and the usage belongs to
	// the moment it was known — which is also the moment Retry-After counts
	// from.
	c.slot(m.now()).settled += actual
}

// Snapshot reports every tenant's standing, for the console.
func (m *Memory) Snapshot(_ context.Context) []Usage {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Usage, 0, len(m.tenants))
	for name, c := range m.tenants {
		requests, tokens, settled := c.totals(now, m.Window)
		out = append(out, Usage{
			Tenant:             name,
			RequestsThisMinute: requests,
			TokensReserved:     tokens,
			TokensSettled:      settled,
			InFlight:           c.inFlight,
		})
	}
	return out
}

func (m *Memory) policyFor(tenant string) Policy {
	p := m.PolicyFor(tenant)
	if p.RequestsPerMinute < 0 {
		p.RequestsPerMinute = 0
	}
	if p.TokensPerMinute < 0 {
		p.TokensPerMinute = 0
	}
	return p
}

// windowEnd is when the current window frees up, for Retry-After.
func (m *Memory) windowEnd(now time.Time) time.Duration {
	return m.Window - now.Sub(now.Truncate(m.Window))
}

// counterFor returns the tenant's counter, creating it with a fresh ring.
//
// A policy change replaces the policy rather than the counter, so a limit that
// was lowered mid-window takes effect against the tokens already spent in it.
func (m *Memory) counterFor(tenant string, policy Policy) *counter {
	c, ok := m.tenants[tenant]
	if !ok {
		c = &counter{
			policy:  policy,
			buckets: make([]bucket, bucketsPerTenant),
		}
		m.tenants[tenant] = c
		return c
	}
	c.policy = policy
	return c
}
