package ratelimit

import (
	"context"
	"time"
)

// Snapshot reports every scope's standing, for the console.
//
// Both levels are reported as their own rows. Summing the projects into the
// tenant row would make it impossible to see which partition is full, and a
// tenant row that is merely the sum of its children is not something a
// console can draw a limit line against.
func (m *Memory) Snapshot(_ context.Context) []Usage {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Usage, 0, len(m.scopes))
	for key, c := range m.scopes {
		requests, tokens, settled := c.totals(now, m.Window)
		out = append(out, Usage{
			Scope:              scopeOf(key),
			Kind:               kindOf(key),
			RequestsThisMinute: requests,
			TokensReserved:     tokens,
			TokensSettled:      settled,
			InFlight:           c.inFlight,
		})
	}
	return out
}

// windowEnd is when the current window frees up, for Retry-After.
func (m *Memory) windowEnd(now time.Time) time.Duration {
	return m.Window - now.Sub(now.Truncate(m.Window))
}
