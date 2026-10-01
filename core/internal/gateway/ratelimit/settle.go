package ratelimit

import "context"

// Settle converts a reservation into settled usage.
//
// The whole reservation is removed from both buckets and the actual amount is
// recorded instead, rather than "refunding the difference". Those two are the
// same arithmetic when actual <= reserved and not the same when it is greater:
// refunding a negative difference leaves the original deduction in place *and*
// adds the actual, so a caller whose engine reported more than reserved would
// be charged for both. Removing all of it and recording actual is correct in
// both directions.
//
// The two levels settle independently, and a scope charged only to its envelope
// settles only there — otherwise a project with no limit of its own would
// accumulate a second, unlimited counter that no policy ever checks.
func (m *Memory) Settle(_ context.Context, r Reservation, actual int) {
	if !r.counted && actual <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	scope := Scope{Tenant: r.tenant, Project: r.project}
	envelope, ok := m.scopes[scope.TenantKey()]
	if !ok {
		return
	}
	m.settleOne(envelope, r, actual)

	if r.project != "" {
		if partition, ok := m.scopes[scope.Key()]; ok {
			m.settleOne(partition, r, actual)
		}
	}
}

func (m *Memory) settleOne(c *counter, r Reservation, actual int) {
	if c.inFlight > 0 {
		c.inFlight--
	}
	now := m.now()

	// Settling to zero means the request never reached an engine — a routing
	// miss, or a replica that refused. It gives back both dimensions, not just
	// the tokens: a request that consumed no GPU second is not a request, and
	// charging it against the per-minute request count would let a caller burn
	// a minute's allowance on a mistyped model name.
	if actual <= 0 {
		if r.counted {
			c.release(r.Reserved, 1, now)
		}
		return
	}
	if r.counted && r.Reserved > 0 {
		// Oldest first: the deduction was recorded when the request arrived,
		// and that is the bucket furthest along towards falling out of the
		// window, so removing it there expires the amount soonest.
		c.release(r.Reserved, 0, now)
	}
	// The actual usage goes into the ring rather than a scalar, so it expires
	// with the window like everything else. A scalar would make a per-minute
	// ceiling into a lifetime one: the caller would be refused forever after
	// spending its allowance once, and only a restart would clear it.
	//
	// It is recorded against the current second, not the second the request
	// arrived, because a long generation spans seconds and the usage belongs to
	// the moment it was known — which is also the moment Retry-After counts
	// from.
	c.slot(now).settled += actual
}
