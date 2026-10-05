// Package ratelimit enforces per-tenant and per-project limits by reserving
// before the request is forwarded and settling after the engine reports actual
// usage.
//
// The reserve-then-settle shape is not an optimisation, it is the only correct
// one under concurrency. A check-then-act limiter ("is usage below the limit?
// yes, then go") lets every request in a burst pass the check simultaneously
// and overshoot the limit by the size of the burst. Reservation deducts inside
// the same lock that reads the counter, so at most the limit is ever committed.
//
// Limits bind to a tenant and a project, never to a key. A key is a
// credential: it rotates, it leaks, and it is not a budget. Binding a limit to
// one would either be unenforceable — the tenant mints another key — or would
// make the limit depend on a secret's lifetime, so lowering a budget would
// require a rotation. See authn for the credential side.
package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Policy is one scope's limits. Zero means unlimited.
//
// It is an alias of authn.Limits rather than a second definition of the same
// two numbers. A limit is declared in configuration and consumed by the
// limiter, and a package that redefined the shape would let the two drift —
// the parser would accept `rpm` and the limiter would read something else.
// The helpers live on authn.Limits for the ordinary Go reason: a method cannot
// be added to a type alias from outside the declaring package.
type Policy = authn.Limits

// Request is what a caller wants to spend.
type Request struct {
	// Scope is who is spending, and in which partition. Both levels are
	// checked: the tenant envelope caps the total, the project limit divides
	// it. See Scope for why one level alone is not enough.
	Scope Scope
	// Tokens is the amount to reserve: the prompt estimate plus the requested
	// maximum completion. It is an upper bound on what the request will cost,
	// which is the point.
	Tokens int
}

// Limiter reserves and settles.
type Limiter interface {
	// Reserve commits the deduction against the tenant envelope and the project
	// partition, or returns a RateLimited error naming which scope was
	// exhausted and when the window frees up. A refusal by either level commits
	// neither.
	Reserve(ctx context.Context, req Request) (Reservation, error)
	// Settle refunds the difference between what was reserved and what the
	// engine reported. Actual > Reserved is possible and is deliberately not
	// clamped: pretending otherwise would make the ledger disagree with the
	// engine, and P6 says the engine is the authority on usage.
	Settle(ctx context.Context, r Reservation, actual int)
	// Snapshot reports current usage, for the console and for /fleet/status.
	Snapshot(ctx context.Context) []Usage
}

// Usage is one scope's current standing.
type Usage struct {
	// Scope is whose counter this is. A scope with no project is a tenant
	// envelope and its figures cover every project inside that tenant; a scope
	// with one is a partition, and the envelope is reported separately rather
	// than summed into it.
	Scope Scope
	// Kind distinguishes the two, so a console can label a row without
	// guessing from whether Project happens to be empty.
	Kind Kind
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

// Kind says which of the two limits a Usage row belongs to.
type Kind string

const (
	// KindTenant is the envelope: everything the tenant spent, across all of
	// its projects.
	KindTenant Kind = "tenant"
	// KindProject is one partition inside a tenant.
	KindProject Kind = "project"
)

// Tenant is a convenience for a scope that names no project.
func Tenant(name string) Scope { return Scope{Tenant: name} }

// Project is a convenience for a scope that names both.
func Project(tenant, project string) Scope {
	return Scope{Tenant: tenant, Project: project}
}

// Limited is a refusal carrying when to try again.
//
// It is a distinct type rather than a sentence the caller has to parse: a
// handler that recovers the retry time from the message breaks the first time
// somebody rewords the message, and it fails as a client that does not honour
// Retry-After and therefore retries in a tight loop.
//
// Only RetryAfter is read by this codebase today. The rest is the payload a
// caller gets instead of parsing Err, and is kept for that reason rather than
// for a reader that has not been written yet.
type Limited struct {
	// Scope is the level that refused. A caller that must tell a tenant their
	// own budget apart from one project's budget needs this, and the message
	// alone is not a reliable place to carry it.
	Scope Scope
	// Kind is whether the tenant envelope or a project partition refused.
	Kind Kind
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
func limited(scope Scope, kind Kind, what string, used, limit int, reset time.Duration) error {
	secs := int(reset.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	// The scope is named in the message because a caller reading a 429 needs to
	// know which budget ran out. "request rate limit reached" on its own would
	// send them to the wrong one.
	return &Limited{
		Scope:      scope,
		Kind:       kind,
		What:       what,
		Used:       used,
		Limit:      limit,
		RetryAfter: secs,
		Err: errs.RateLimited("%s %s %s limit reached: %d of %d used; retry in %ds",
			scope, kind, what, used, limit, secs),
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

// LimitedError builds a refusal naming which scope ran out and when to come
// back.
//
// Exported for the same reason NewReservation is: a Limiter implemented
// outside this package has to produce this type, and building it field by field
// would put the message format in two places, where the second copy is the one
// nobody updates.
func LimitedError(scope Scope, kind Kind, what string, used, limit int, reset time.Duration) error {
	return limited(scope, kind, what, used, limit, reset)
}
