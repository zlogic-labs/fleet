package ratelimit

import (
	"context"
	"testing"
	"time"
)

// The shared fixtures.
//
// Split out from the tests themselves so that a file about the two-level
// check does not also carry the clock helpers, and so that a new test file does
// not have to re-declare them.

// fixedClock lets the sliding window be driven instead of waited out.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(clock *fixedClock, p Policies) *Memory {
	m := NewMemory(func(Scope) Policies { return p })
	m.now = clock.now
	return m
}

func tenantScope() Scope       { return Tenant("acme") }
func projectScope() Scope      { return Project("acme", "research") }
func otherProjectScope() Scope { return Project("acme", "batch") }

func usageFor(t *testing.T, m *Memory, want Scope) Usage {
	t.Helper()
	for _, u := range m.Snapshot(context.Background()) {
		if u.Scope == want {
			return u
		}
	}
	t.Fatalf("no usage reported for scope %s", want)
	return Usage{}
}

// asLimited is errors.As for the one type these tests care about, written out
// so a failure message names the scope rather than a wrapped chain.
func asLimited(err error, dst **Limited) bool {
	l, ok := err.(*Limited)
	if ok {
		*dst = l
	}
	return ok
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
