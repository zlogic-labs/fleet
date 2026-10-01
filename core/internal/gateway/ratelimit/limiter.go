// Package ratelimit enforces per-tenant limits by reserving before the request
// is forwarded and settling after the engine reports actual usage.
//
// The reserve-then-settle shape is not an optimisation, it is the only correct
// one under concurrency. A check-then-act limiter ("is usage below the limit?
// yes, then go") lets every request in a burst pass the check simultaneously
// and overshoot the limit by the size of the burst. Reservation deducts inside
// the same lock that reads the counter, so at most the limit is ever committed.
package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Reservation is a committed deduction awaiting settlement.
//
// It is a value rather than a token because the caller holds exactly one and
// passes it back by value; the shared state lives in the limiter's own map
// keyed by an opaque id. That keeps the thing that crosses package boundaries
// trivially copyable, and keeps the mutex out of every caller's struct.
type Reservation struct {
	id     string
	tenant string
	policy Policy
	// Reserved is what was deducted. Settlement removes all of it and records
	// the actual amount instead.
	Reserved int
	// counted records whether this reservation took a request slot. It is false
	// for an unlimited policy, which returns before touching any counter, and
	// settlement must not then credit back a request that was never counted.
	counted bool
}

// ID identifies the reservation for logs.
func (r Reservation) ID() string { return r.id }

// TenantName is who the reservation is charged to.
func (r Reservation) TenantName() string { return r.tenant }

// Policy is the limits the reservation was taken under.
func (r Reservation) Limits() Policy { return r.policy }

// Policy is a tenant's limits. Zero means unlimited.
//
// It is an alias of authn.Limits rather than a second definition of the same
// two numbers. A limit is declared on a key and consumed by the limiter, and a
// package that redefined the shape would let the two drift — the key would
// accept `rpm` and the limiter would read something else. The helpers live on
// authn.Limits for the ordinary Go reason: a method cannot be added to a type
// alias from outside the declaring package.
type Policy = authn.Limits

// Request is what a caller wants to spend.
type Request struct {
	// Tenant is who is spending. Requests are limited per tenant rather than
	// per key, because the resource being limited — GPU·hours — is bought per
	// tenant and a tenant with ten keys has not bought ten engines.
	Tenant string
	// Tokens is the amount to reserve: the prompt estimate plus the requested
	// maximum completion. It is an upper bound on what the request will cost,
	// which is the point.
	Tokens int
}

// Limiter reserves and settles.
type Limiter interface {
	// Reserve commits the deduction, or returns a RateLimited error naming
	// what was exhausted and when the window frees up.
	Reserve(ctx context.Context, req Request) (Reservation, error)
	// Settle refunds the difference between what was reserved and what the
	// engine reported. Actual > Reserved is possible and is deliberately not
	// clamped: pretending otherwise would make the ledger disagree with the
	// engine, and P6 says the engine is the authority on usage.
	Settle(ctx context.Context, r Reservation, actual int)
	// Snapshot reports current usage, for the console and for /fleet/status.
	Snapshot(ctx context.Context) []Usage
}

// Usage is one tenant's current standing.
type Usage struct {
	Tenant string
	// RequestsThisMinute is what has been committed over the window, including
	// requests still in flight and not yet settled. The two token figures split
	// the same total by state: TokensReserved is what is held by requests that
	// have not finished, TokensSettled what they actually cost.
	RequestsThisMinute int
	TokensReserved     int
	TokensSettled      int
	// InFlight is how many reservations are outstanding. It is not windowed —
	// a request either is running or is not.
	InFlight int
}

// Limited is a refusal carrying when to try again.
//
// It is a distinct type rather than a sentence the caller has to parse: a
// handler that recovers the retry time from the message breaks the first time
// somebody rewords the message, and it fails as a client that does not honour
// Retry-After and therefore retries in a tight loop.
type Limited struct {
	// What names the exhausted dimension: "request rate" or "token rate".
	What  string
	Used  int
	Limit int
	// RetryAfter is how long until the window frees up, in seconds, minimum 1.
	RetryAfter int
	Err        error
}

func (l *Limited) Error() string { return l.Err.Error() }
func (l *Limited) Unwrap() error { return l.Err }

// limited builds the refusal.
//
// The HTTP header is set by the handler, not here: this package does not know
// about HTTP, and a limiter that could write a header would be one more thing
// to get wrong on a path that has to be fast.
func limited(what string, used, limit int, reset time.Duration) error {
	secs := int(reset.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return &Limited{
		What:       what,
		Used:       used,
		Limit:      limit,
		RetryAfter: secs,
		Err: errs.RateLimited("%s limit reached: %d of %d used; retry in %ds",
			what, used, limit, secs),
	}
}

// RetryAfterSeconds returns the retry delay a refusal carries, or "" when the
// error is not a rate limit.
func RetryAfterSeconds(err error) string {
	var l *Limited
	if errors.As(err, &l) {
		return strconv.Itoa(l.RetryAfter)
	}
	return ""
}
