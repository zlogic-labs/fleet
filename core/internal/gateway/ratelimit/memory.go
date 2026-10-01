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
// Correct under concurrency because Reserve takes one lock and decides both
// levels inside it. That single lock is what makes the two-level check sound:
// checking the tenant envelope, releasing, then checking the project partition
// is a check-then-act with extra steps, and a burst would slip through the gap
// between the two.
type Memory struct {
	mu sync.Mutex
	// scopes holds one counter per bucket key: a bare tenant for its envelope,
	// "tenant/project" for a partition. One map rather than two so a request
	// resolves both under the same lock.
	scopes map[string]*counter

	// PolicyFor supplies the limits for a scope, so a limit can be changed in
	// configuration without restarting every in-flight request's accounting.
	// Returning unlimited policies disables the limiter for that scope.
	PolicyFor func(Scope) Policies

	// Window is the period the limit applies over.
	Window time.Duration

	// now is a field so a test can drive time instead of sleeping. A limiter
	// whose reset behaviour can only be observed by waiting a minute is a
	// limiter whose reset behaviour goes untested.
	now func() time.Time
	seq atomic.Uint64
}

// Policies are the two limits in force for one scope.
//
// A struct rather than two bare Limits so that "this project has no limit of
// its own" is representable. A project that only says "no limit" still runs
// under the tenant envelope, and a single zero-valued Limits on both levels
// could not distinguish that from a project that is not being limited at all.
type Policies struct {
	// Envelope is the tenant's total. It applies to every request from the
	// tenant regardless of project.
	Envelope Policy
	// Partition is the project's own limit inside the envelope.
	Partition Policy
}

// Unlimited reports whether neither level limits anything.
func (p Policies) Unlimited() bool { return p.Envelope.Unlimited() && p.Partition.Unlimited() }

// DefaultWindow is the period RequestsPerMinute and TokensPerMinute apply
// over.
const DefaultWindow = time.Minute

func NewMemory(policyFor func(Scope) Policies) *Memory {
	m := &Memory{
		scopes:    map[string]*counter{},
		Window:    DefaultWindow,
		now:       time.Now,
		PolicyFor: policyFor,
	}
	if policyFor == nil {
		m.PolicyFor = func(Scope) Policies { return Policies{} }
	}
	return m
}

var _ Limiter = (*Memory)(nil)

// Reserve commits the deduction for one request against both levels.
//
// The order is envelope first, then partition, and it matters only for which
// refusal a caller sees when both are exhausted — the request is refused either
// way. Checking the envelope first means the answer in the common case is the
// tenant's own budget rather than whichever project happened to run out first.
//
// Neither counter is touched until both checks pass. A refusal that had already
// incremented the envelope would charge the tenant for a request that never
// ran.
func (m *Memory) Reserve(_ context.Context, req Request) (Reservation, error) {
	scope := req.Scope.Normalized()
	policies := m.policiesFor(scope)
	if policies.Unlimited() {
		return Reservation{
			tenant:        scope.Tenant,
			project:       scope.Project,
			tenantPolicy:  policies.Envelope,
			projectPolicy: policies.Partition,
		}, nil
	}

	now := m.now()
	reset := m.windowEnd(now)

	m.mu.Lock()
	defer m.mu.Unlock()

	envelope := m.counterFor(scope.TenantKey(), Policies{Envelope: policies.Envelope}.Envelope)
	partition := envelope
	if scope.Project != "" {
		partition = m.counterFor(scope.Key(), policies.Partition)
	}

	if err := admit(envelope, policies.Envelope, now, m.Window, reset, scope, KindTenant, req.Tokens); err != nil {
		return Reservation{}, err
	}
	if partition != envelope {
		if err := admit(partition, policies.Partition, now, m.Window, reset, scope, KindProject, req.Tokens); err != nil {
			// The envelope passed but the partition refused, so nothing is
			// committed. Without this the tenant would be billed for every
			// request its fullest project rejects — which is exactly the
			// request volume it cannot control.
			envelope.release(req.Tokens, 1, now)
			return Reservation{}, err
		}
	}

	return Reservation{
		id:            fmt.Sprintf("rsv-%d-%s", m.seq.Add(1), scope),
		tenant:        scope.Tenant,
		project:       scope.Project,
		tenantPolicy:  policies.Envelope,
		projectPolicy: policies.Partition,
		Reserved:      req.Tokens,
		counted:       true,
	}, nil
}

// admit checks one level and, if it fits, commits the deduction.
//
// The figure compared against the token ceiling is reserved PLUS settled, not
// reserved alone. Settling moves a request's tokens out of the reservation and
// into the settled total, so a check that read only the reservation would see
// the bucket drain to nothing after every request completes — and a caller
// sending a steady stream of short requests would never reach its ceiling at
// all. Both halves are read from the ring, so both leave when the window rolls
// over.
func admit(c *counter, policy Policy, now time.Time, window, reset time.Duration, scope Scope, kind Kind, tokens int) error {
	requests, reserved, settled := c.totals(now, window)

	if policy.RequestsPerMinute > 0 && requests >= policy.RequestsPerMinute {
		return limited(scope, kind, "request rate", requests, policy.RequestsPerMinute, reset)
	}
	if policy.TokensPerMinute > 0 {
		committed := reserved + settled
		if committed+tokens > policy.TokensPerMinute {
			return limited(scope, kind, "token rate", committed, policy.TokensPerMinute, reset)
		}
	}

	slot := c.slot(now)
	slot.requests++
	slot.tokens += tokens
	c.inFlight++
	return nil
}

func (m *Memory) policiesFor(scope Scope) Policies {
	p := m.PolicyFor(scope)
	// Negative is nonsense rather than "unlimited": a limit of -1 reads as
	// unlimited in a config file, which is a limit nobody wrote on purpose.
	if p.Envelope.RequestsPerMinute < 0 {
		p.Envelope.RequestsPerMinute = 0
	}
	if p.Envelope.TokensPerMinute < 0 {
		p.Envelope.TokensPerMinute = 0
	}
	if p.Partition.RequestsPerMinute < 0 {
		p.Partition.RequestsPerMinute = 0
	}
	if p.Partition.TokensPerMinute < 0 {
		p.Partition.TokensPerMinute = 0
	}
	return p
}

// counterFor returns the bucket's counter, creating it with a fresh ring.
//
// A policy change replaces the policy rather than the counter, so a limit that
// was lowered mid-window takes effect against the tokens already spent in it.
func (m *Memory) counterFor(key string, policy Policy) *counter {
	c, ok := m.scopes[key]
	if !ok {
		c = &counter{scope: scopeOf(key), buckets: make([]bucket, bucketsPerScope)}
		m.scopes[key] = c
	}
	c.policy = policy
	return c
}
