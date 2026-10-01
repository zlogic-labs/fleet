package ratelimit

import "time"

// The sliding window.
//
// A ring of one-second buckets, indexed by unix second modulo the ring length,
// so a tenant's last sixty seconds are always exactly where the next second
// will look for them. Nothing is swept and nothing expires on a timer: a
// bucket stops counting the moment it falls outside the window, which is what
// `live` decides, and what `slot` recycles.
//
// The alternative — a fixed window keyed on the current minute — is cheaper by a
// few nanoseconds and has a well-known hole: a caller that spends its whole
// allowance in the last second of a minute gets a fresh one in the first second
// of the next, doubling its rate at will. TestSlidingWindowDoesNotDoubleAllowanceAtTheBoundary
// is the check for that.

// bucketsPerTenant is how many per-second readings a sliding minute keeps.
//
// Sixty means one reading per second, which bounds the error in the reported
// rate to one second's worth of traffic — under a percent for any real tenant
// — while costing 60 small counters per tenant.
const bucketsPerTenant = 60

type bucket struct {
	second   int64
	requests int
	tokens   int
	// settled is the real usage of requests that arrived in this second and
	// have since been settled. It is separate from tokens, which holds the
	// reservations still in flight, because the two are released on different
	// events: a reservation leaves when its request finishes, the settled total
	// leaves when the window rolls over.
	settled int
}

type counter struct {
	policy  Policy
	buckets []bucket
	// inFlight is a scalar rather than a ring entry because it is a point-in-time
	// fact with no window: a request either is in flight or is not, and ageing
	// it out would report work that is still running as though it had finished.
	inFlight int
}

// slot returns the bucket for now's second, recycling the ring entry that
// falls out of the window.
//
// Recycling by second is what makes this a sliding window rather than sixty
// independent counters: the slot for a second is only ever reused once that
// second has aged out of the window entirely, so nothing inside the window is
// ever overwritten.
func (c *counter) slot(now time.Time) *bucket {
	sec := now.Unix()
	b := &c.buckets[int(sec)%len(c.buckets)]
	if b.second != sec {
		b.second, b.requests, b.tokens, b.settled = sec, 0, 0, 0
	}
	return b
}

// live reports whether a bucket's second is still inside the window ending at
// now.
//
// This is the check that makes the ring correct. A bucket whose second is
// exactly Window old has expired; one that is newer has not. Buckets are not
// cleared eagerly, because doing that would need a background sweeper to be
// exact, and a stale-by-one-second bucket contributing nothing is a much
// smaller error than a goroutine per tenant.
func (c *counter) live(b bucket, now time.Time, window time.Duration) bool {
	return now.Unix()-b.second < int64(window/time.Second)
}

// totals sums the buckets still inside the window.
//
// Settled usage is included with the reservations, because the limit is on what
// a tenant is consuming over the window and a token is a token whether or not
// the request that spent it has finished.
func (c *counter) totals(now time.Time, window time.Duration) (requests, tokens, settled int) {
	for _, b := range c.buckets {
		if !c.live(b, now, window) {
			continue
		}
		requests += b.requests
		tokens += b.tokens
		settled += b.settled
	}
	return requests, tokens, settled
}

// release removes a settled reservation from the buckets: n tokens and req
// request counts, oldest first.
//
// Oldest first, so walk the ring backwards from the current second. The bucket
// a reservation was recorded in is the one furthest along towards expiring, so
// removing it there makes the refund disappear with the window instead of
// extending the tenant's usage into a minute it was not spent in.
//
// A reservation may only be released once. Releasing more than was recorded
// would silently credit the tenant, so the caller is trusted to settle each
// reservation exactly once.
func (c *counter) release(n, req int, now time.Time) {
	for i := range c.buckets {
		idx := (int(now.Unix()) - i - 1 + len(c.buckets)) % len(c.buckets)
		b := &c.buckets[idx]
		if b.tokens > 0 && n > 0 {
			take := b.tokens
			if take > n {
				take = n
			}
			b.tokens -= take
			n -= take
		}
		if b.requests > 0 && req > 0 {
			take := b.requests
			if take > req {
				take = req
			}
			b.requests -= take
			req -= take
		}
		if n == 0 && req == 0 {
			return
		}
	}
}
