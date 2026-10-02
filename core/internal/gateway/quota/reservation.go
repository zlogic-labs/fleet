package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// Reservation is a set of committed deductions awaiting settlement.
//
// Opaque and passed straight back, as in ratelimit: the caller holds exactly
// one and the shared state stays on the far side of the interface.
//
// It remembers the rules rather than opaque keys because settlement has to
// re-derive which column each deduction belongs in, and a key that encoded the
// column would have to be parsed back to find out — the rule is the thing that
// already holds that answer.
type Reservation struct {
	id        string
	scope     ratelimit.Scope
	rules     []Rule
	committed []int64
	now       time.Time
}

// NewReservation builds one. The id is derived from the scope and instant
// rather than generated, so two gateways reserving for the same scope in the
// same nanosecond do not collide and nothing has to be handed back to identify
// a settlement.
func NewReservation(scope ratelimit.Scope, rules []Rule, committed []int64, now time.Time) Reservation {
	return Reservation{
		id:        fmt.Sprintf("%s@%d", scope.Key(), now.UnixNano()),
		scope:     scope,
		rules:     rules,
		committed: committed,
		now:       now,
	}
}

// ID identifies this reservation for logging.
func (r Reservation) ID() string { return r.id }

// Scope is who reserved it.
func (r Reservation) Scope() ratelimit.Scope { return r.scope }

// Rules are the rules it was admitted against, in the order Committed matches.
//
// A copy, because the store writes back against these and the caller must not
// be able to change a rule between admitting and settling it.
func (r Reservation) Rules() []Rule {
	out := make([]Rule, len(r.rules))
	copy(out, r.rules)
	return out
}

// Committed is what was taken per rule, which is what settlement gives back —
// not what the request turned out to cost. The two differ whenever the estimate
// was an over-estimate, and refunding the actual instead is how a reservation
// quietly becomes permanent.
func (r Reservation) Committed() []int64 {
	out := make([]int64, len(r.committed))
	copy(out, r.committed)
	return out
}

// At is the clock the bucket was chosen from.
func (r Reservation) At() time.Time { return r.now }

// Limiter is the budget side of reserve-then-settle.
//
// Used is not part of Reserve because a report that silently disagreed with the
// limiter would be worse than no report: it is what a tenant compares their
// dashboard against, and the two reading different windows is a support
// question nobody can answer.
type Limiter interface {
	Reserve(ctx context.Context, req Request) (Reservation, error)
	Settle(ctx context.Context, r Reservation, actual Estimate)
	Used(ctx context.Context, scope ratelimit.Scope) (map[Dimension]int64, error)
}

// Exceeded is a refusal.
//
// A typed error rather than a message a caller parses: the HTTP status, the
// Retry-After and the "which rule" answer all come off the fields, and three
// callers reading three things out of a sentence is three ways for them to
// disagree.
type Exceeded struct {
	Scope     ratelimit.Scope
	Kind      ScopeKind
	Dimension Dimension
	// Used and Limit are in the dimension's own unit, so the message needs no
	// explanation of which scale it is on.
	Used     int64
	Limit    int64
	Window   time.Duration
	ResetsAt time.Time
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("%s %s budget exhausted: %d of %d used over the last %s; resets at %s",
		e.Scope, e.Dimension, e.Used, e.Limit, e.Window, e.ResetsAt.Format(time.RFC3339))
}

// RetryAfter is a duration string for the HTTP header.
//
// The window is rolling, so "when it resets" is the moment the oldest spending
// falls out of it rather than a boundary to wait for. Reporting the full
// window would be a lie to a client that took the hint.
func (e *Exceeded) RetryAfter() string {
	d := time.Until(e.ResetsAt)
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", int(d.Seconds()))
}

// AsExceeded returns the refusal in err, or nil.
//
// errors.As because the error will have been wrapped by whatever logged it on
// the way out, and a gateway that could only recognise its own unwrapped errors
// would answer 500 to an ordinary budget exhaustion.
func AsExceeded(err error) *Exceeded {
	var e *Exceeded
	if !errors.As(err, &e) {
		return nil
	}
	return e
}
