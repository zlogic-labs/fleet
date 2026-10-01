package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestTenantsAreLimitedIndependently(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{RequestsPerMinute: 3}})

	for i := 0; i < 3; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 1}); err != nil {
			t.Fatalf("acme %d: %v", i, err)
		}
	}
	if _, err := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 1}); err == nil {
		t.Error("acme must be at its limit")
	}
	if _, err := m.Reserve(ctx, Request{Scope: Tenant("other"), Tokens: 1}); err != nil {
		t.Errorf("a second tenant must not inherit the first one's usage: %v", err)
	}
}

// An unlimited policy is the honest reading of "nothing is being charged", and
// it is what keeps a gateway with authentication disabled from being turned
// into a shared-bucket denial of service by one anonymous caller.
func TestUnlimitedPolicyAdmitsEverything(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{})

	for i := 0; i < 1000; i++ {
		if _, err := m.Reserve(ctx, Request{Scope: Anonymous, Tokens: 1_000_000}); err != nil {
			t.Fatalf("request %d refused under an unlimited policy: %v", i, err)
		}
	}
}

func TestInFlightIsTrackedAndReleased(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{RequestsPerMinute: 100}})

	a, _ := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 10})
	b, _ := m.Reserve(ctx, Request{Scope: tenantScope(), Tokens: 10})
	if got := usageFor(t, m, tenantScope()).InFlight; got != 2 {
		t.Errorf("in flight = %d, want 2", got)
	}
	m.Settle(ctx, a, 10)
	m.Settle(ctx, b, 10)
	if got := usageFor(t, m, tenantScope()).InFlight; got != 0 {
		t.Errorf("in flight = %d, want 0 after both settled", got)
	}
}

// A report has to name the two levels, or a console cannot draw a tenant's
// total next to the partition that filled up.
func TestSnapshotLabelsBothLevels(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{
		Envelope:  Policy{RequestsPerMinute: 100},
		Partition: Policy{RequestsPerMinute: 100},
	})

	if _, err := m.Reserve(ctx, Request{Scope: projectScope(), Tokens: 7}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	var sawTenant, sawProject bool
	for _, u := range m.Snapshot(ctx) {
		if u.Scope.Tenant != "acme" {
			continue
		}
		switch u.Kind {
		case KindTenant:
			sawTenant = true
			if u.Scope.Project != "" {
				t.Errorf("tenant row carries project %q", u.Scope.Project)
			}
		case KindProject:
			sawProject = true
			if u.Scope.Project != "research" {
				t.Errorf("project row = %+v, want research", u.Scope)
			}
			if u.TokensReserved != 7 {
				t.Errorf("project row reserved = %d, want 7", u.TokensReserved)
			}
		}
	}
	if !sawTenant || !sawProject {
		t.Errorf("snapshot reported tenant=%v project=%v, want both", sawTenant, sawProject)
	}
}

// Two spellings of one scope must reach one bucket, or each gets a full
// allowance — the same tenant would be able to double its own budget by
// changing the case in its key.
func TestScopeNormalisationIsShared(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	m := newTestLimiter(clock, Policies{Envelope: Policy{RequestsPerMinute: 2}})

	if _, err := m.Reserve(ctx, Request{Scope: Scope{Tenant: "  Acme "}, Tokens: 1}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := m.Reserve(ctx, Request{Scope: Scope{Tenant: "ACME"}, Tokens: 1}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := m.Reserve(ctx, Request{Scope: Scope{Tenant: "acme"}, Tokens: 1}); err == nil {
		t.Error("a second spelling of the same tenant got its own budget")
	}
}
