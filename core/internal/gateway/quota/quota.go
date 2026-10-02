// Package quota enforces a tenant's and a project's spend budget by reserving
// before the request is forwarded and settling after the charge is known.
//
// It is the same reserve-then-settle discipline as package ratelimit, and for
// the same reason: a check-then-act budget lets a burst of requests all read
// "under budget" at once and collectively spend several times it. Reserving
// makes at most the budget ever committed.
//
// What differs from a rate limit is what the counters are worth. A rate limit
// is a gate and may forget itself; a budget is money and may not. So the
// implementation is durable, and it is not in package ratelimit even though the
// shape is identical — folding them together would mean the limiter's in-memory
// fast path became the budget's, and the budget would lose a restart's worth of
// enforcement every time the process bounced.
//
// Limits bind to a tenant and a project, never to a key, for the reason given
// in ratelimit: a key is a credential, not a budget.
package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Window is the period a budget is measured over.
//
// It is passed in rather than computed here. Which window a tenant is billed
// over is still an open decision (docs/architecture.md §11): a calendar month
// is obvious but punishes a tenant that happened to start using Fleet on the
// 31st, and a rolling window is fair but makes an invoice non-reproducible from
// a date range. Putting the window in the signature keeps that decision in one
// place instead of in every query that touches a counter.
type Window struct {
	From time.Time
	To   time.Time
}

// Period maps an instant to the window it falls in. The gateway supplies one;
// tests supply a fixed one.
type Period func(now time.Time) Window

// CalendarMonth is the default period: the UTC calendar month containing now.
//
// UTC rather than local because a tenant's budget must not depend on where the
// gateway happens to run, and two gateways in two timezones must not disagree
// about how much of an allowance is gone.
func CalendarMonth(now time.Time) Window {
	start := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	return Window{From: start, To: start.AddDate(0, 1, 0)}
}

// Request is one request's upper bound on cost.
type Request struct {
	Scope ratelimit.Scope
	// Bound is the most this request could cost. It must be an upper bound,
	// not an estimate of what will happen: the whole point of reserving before
	// forwarding is that the real charge is not known yet.
	//
	// Zero is a refusal to reserve anything, which for an unpriced model means
	// the budget is not enforced at all for that model. Callers must decide
	// what zero means rather than let it pass silently — see the gateway's
	// settle path.
	Bound billing.Amount
	Now   time.Time
}

// Reservation is a committed deduction awaiting settlement.
//
// As in ratelimit it is an opaque value the caller passes straight back, so the
// shared state stays on the far side of the interface. That is why it has a
// constructor: a limiter is the only thing that knows how a deduction is
// recorded, and a caller assembling one by field would be depending on that
// internals.
type Reservation struct {
	id     string
	tenant string
	// project is the partition charged, empty when the request was charged to
	// the tenant envelope alone.
	project string
	// Both levels reserved the same bound; they are tracked separately so
	// settlement can release exactly what each one took, and so a level that
	// was unlimited can be told apart from one that was zero.
	tenantBound  billing.Amount
	projectBound billing.Amount
	// envelope and partition are the limits in force when the reservation was
	// taken, kept for reporting. Settlement does not re-read them: a budget
	// lowered mid-flight must not change what an in-flight request releases.
	envelope  billing.Amount
	partition billing.Amount
	window    Window
}

// NewReservation builds one for a limiter that has just committed it.
//
// Exported because the implementation lives in another package, and unexported
// fields because nothing else should be deciding what a deduction looks like.
func NewReservation(scope ratelimit.Scope, bound, envelope, partition billing.Amount, w Window) Reservation {
	tenantBound, projectBound := bound, bound
	if envelope <= 0 {
		tenantBound = 0
	}
	if partition <= 0 {
		projectBound = 0
	}
	return Reservation{
		id:           fmt.Sprintf("%s/%d/%d", scope.Key(), w.From.Unix(), bound),
		tenant:       scope.Tenant,
		project:      scope.Project,
		tenantBound:  tenantBound,
		projectBound: projectBound,
		envelope:     envelope,
		partition:    partition,
		window:       w,
	}
}

// TenantBound is what the tenant envelope reserved.
func (r Reservation) TenantBound() billing.Amount { return r.tenantBound }

// ProjectBound is what the project partition reserved.
func (r Reservation) ProjectBound() billing.Amount { return r.projectBound }

// Envelope is the tenant budget in force at reservation time.
func (r Reservation) Envelope() billing.Amount { return r.envelope }

// Partition is the project budget in force at reservation time.
func (r Reservation) Partition() billing.Amount { return r.partition }

// ID identifies the reservation for logs.
func (r Reservation) ID() string { return r.id }

// Scope names what was charged.
func (r Reservation) Scope() ratelimit.Scope {
	return ratelimit.Scope{Tenant: r.tenant, Project: r.project}
}

// Bound is the total reserved across both levels.
func (r Reservation) Bound() billing.Amount {
	return r.tenantBound + r.projectBound
}

// Window is the period the reservation was taken against.
func (r Reservation) Window() Window { return r.window }

// Limiter reserves and settles a budget.
//
// Every method takes a context and every implementation is expected to honour
// it: the reservation happens on the request path, after the response has gone
// out, and a database that ignores cancellation turns a stalled database into a
// stalled gateway.
type Limiter interface {
	// Reserve commits an upper bound, or refuses it.
	//
	// Refusing means the budget cannot cover the bound. It is not a partial
	// commit: a reservation that could only afford half is refused entirely,
	// because a request priced at its upper bound and then settling for less
	// would have been fine, and refusing it teaches the tenant that Fleet
	// stops working rather than that it is out of budget.
	Reserve(ctx context.Context, req Request) (Reservation, error)

	// Settle replaces a reservation with the real charge.
	//
	// Actual may exceed the bound when the engine ignores max_tokens or when
	// the prompt was longer than estimated. It must never be dropped for
	// exceeding the bound: the tokens were spent, and the budget has to show
	// it, or the next reconciliation finds a bill nobody was charged.
	Settle(ctx context.Context, r Reservation, actual billing.Amount)

	// Spent reports committed spend for a scope over a window, for the control
	// plane and for the tests that check the arithmetic.
	Spent(ctx context.Context, scope ratelimit.Scope, w Window) (billing.Amount, error)
}

// Exceeded is a refusal.
//
// It is a type rather than a formatted message so the gateway can answer with
// the right status and the right Retry-After without parsing prose — and so a
// client can tell "you are out of budget" apart from "you are going too fast",
// which are different problems with different remedies.
type Exceeded struct {
	Scope ratelimit.Scope
	// Kind is which of the two counters ran out.
	Kind     Kind
	Spent    billing.Amount
	Budget   billing.Amount
	ResetsAt time.Time
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("%s budget exhausted: %s of %s spent; resets at %s",
		e.Scope, e.Spent, e.Budget, e.ResetsAt.Format(time.RFC3339))
}

// RetryAfter is a duration string for the HTTP header, or "" when the reset
// moment is already in the past.
func (e *Exceeded) RetryAfter() string {
	d := time.Until(e.ResetsAt)
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", int(d.Seconds()))
}

// AsExceeded returns the refusal in err, or nil.
//
// errors.As rather than a type assertion because the error will have been
// wrapped by whatever logged it on the way out, and a gateway that could only
// recognise its own unwrapped errors would answer 500 to a perfectly ordinary
// budget exhaustion.
func AsExceeded(err error) *Exceeded {
	var e *Exceeded
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// Kind says which counter refused.
type Kind string

const (
	// KindEnvelope is the tenant's total across all its projects.
	KindEnvelope Kind = "tenant"
	// KindPartition is one project's slice of the envelope.
	KindPartition Kind = "project"
)
