package ratelimit

import (
	"context"
	"testing"
	"time"
)

// The two-level check: a tenant envelope and a project partition, both applied
// to every request. These are the tests that would fail if the two were merged
// into one figure, which is the single easiest mistake to make here and the one
// that leaves the envelope decorative.

// The reason there are two counters at all.
//
// A per-project limit checked on its own is arithmetic the tenant performs: ten
// projects at 300 requests per minute is 3000, and no per-project number would
// ever have bound. The tenant envelope is what makes the partitions add up, so
// this asserts that spending one project's full allowance leaves the others
// unable to spend anything.
func TestTenantEnvelopeBoundsAllProjectsTogether(t *testing.T) {
	ctx := context.Background()
	const limit = 10
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{RequestsPerMinute: limit},
		Partition: Policy{RequestsPerMinute: limit},
	})

	// Spend research's entire allowance on one project.
	for i := 0; i < limit; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1}); err != nil {
			t.Fatalf("research %d: %v", i, err)
		}
	}
	if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1}); err == nil {
		t.Fatal("research is at its own limit and must be refused")
	}

	// A different project of the same tenant must also be refused. Each has a
	// fresh partition counter, so only the shared envelope can stop this.
	if _, err := m.Reserve(ctx, Request{Scope: otherProjectScope(), Tokens: 1}); err == nil {
		t.Fatal("a second project must not be able to spend past the tenant envelope")
	}

	var limitedErr *Limited
	_, err := m.Reserve(ctx, Request{Scope: otherProjectScope(), Tokens: 1})
	if !asLimited(err, &limitedErr) {
		t.Fatalf("err = %v, want a *Limited", err)
	}
	if limitedErr.Kind != KindTenant {
		t.Errorf("refused by %q, want the tenant envelope — the partition was empty", limitedErr.Kind)
	}
	if limitedErr.Scope.Tenant != "acme" {
		t.Errorf("refusal scope = %+v, want tenant acme", limitedErr.Scope)
	}
}

// The inverse of the above: a project that is full is refused by its own limit
// while the tenant envelope still has room, and the message has to say which
// budget ran out. A caller told only "rate limit reached" would go looking in
// the wrong place.
func TestProjectLimitRefusesWhileTheEnvelopeHasRoom(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{RequestsPerMinute: 100},
		Partition: Policy{RequestsPerMinute: 2},
	})

	for i := 0; i < 2; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1}); err != nil {
			t.Fatalf("research %d: %v", i, err)
		}
	}
	_, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1})
	var limitedErr *Limited
	if !asLimited(err, &limitedErr) {
		t.Fatalf("err = %v, want a *Limited", err)
	}
	if limitedErr.Kind != KindProject {
		t.Errorf("refused by %q, want the project partition", limitedErr.Kind)
	}
	if limitedErr.Scope.Project != "research" {
		t.Errorf("refusal scope = %+v, want project research", limitedErr.Scope)
	}
	// The sibling project still works: it has its own partition and the
	// envelope is untouched.
	if _, err := m.Reserve(ctx, Request{Scope: otherProjectScope(), Tokens: 1}); err != nil {
		t.Errorf("a sibling project must not inherit research's refusal: %v", err)
	}
}

// A project refused by its own limit must not cost the tenant anything. If the
// envelope kept the deduction, a tenant whose fullest project is being turned
// away would be charged for the refusals — the one part of its traffic it does
// not control.
func TestRefusedByPartitionDoesNotSpendTheEnvelope(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{RequestsPerMinute: 100},
		Partition: Policy{RequestsPerMinute: 1},
	})

	if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Five more attempts, all refused by the partition.
	for i := 0; i < 5; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 1}); err == nil {
			t.Fatalf("attempt %d should have been refused", i)
		}
	}

	// The envelope has only seen the one request that was admitted.
	if got := usageFor(t, m, tenantScope()).RequestsThisMinute; got != 1 {
		t.Errorf("envelope counted %d requests, want 1 — a refusal must not spend the tenant's budget", got)
	}
}

// Settlement has to give the money back twice, once per bucket. Releasing only
// the envelope would let a project spend its limit forever without any of it
// returning, which turns a per-minute ceiling into a lifetime one.
func TestSettleRefundsBothLevels(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{TokensPerMinute: 1000},
		Partition: Policy{TokensPerMinute: 1000},
	})

	for i := 0; i < 2; i++ {
		r, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 400})
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		m.Settle(ctx, r, 40)
	}

	// After refunds both buckets hold 80, so 800 more fits. If either level had
	// kept its deduction the third reservation would be refused.
	if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 800}); err != nil {
		t.Fatalf("after settling 2x400 to 40 each, 800 more must fit: %v", err)
	}
	for _, s := range []Scope{tenantScope(), projectScope()} {
		if got := usageFor(t, m, s).TokensSettled; got != 80 {
			t.Errorf("%s settled = %d, want 80", s, got)
		}
	}
}

// A project with no limit of its own still runs under the tenant envelope, and
// must not accumulate a second counter nothing reads.
func TestProjectWithoutItsOwnLimitIsBoundedByTheEnvelope(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := NewMemory(func(Scope) Policies {
		return Policies{Envelope: Policy{RequestsPerMinute: 2}}
	})
	m.now = clock.now

	for i := 0; i < 2; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: Project("acme", "loose"), Tokens: 1}); err != nil {
			t.Fatalf("loose %d: %v", i, err)
		}
	}
	if _, err := m.Reserve(ctx, Request{Scope: Project("acme", "loose"), Tokens: 1}); err == nil {
		t.Error("a project with no limit of its own must still be bounded by the envelope")
	}
}
