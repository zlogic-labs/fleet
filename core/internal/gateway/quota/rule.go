package quota

import (
	"fmt"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// A budget is a set of rules. Each is "this much of this dimension per this
// much time", and a request is refused as soon as one of them would be passed.
//
// Rules are separate rather than combined into a single allowance on purpose.
// "5M tokens per 5 hours" and "200 units per month" are not in tension — they
// are two different resources, and a tenant under one of them may be far over
// the other. Collapsing them would mean inventing a conversion between tokens
// and money, which is exactly the number P8 says cannot be known before the
// month's cost pool is settled.

// Rule is one cap on one scope.
type Rule struct {
	ScopeKind ScopeKind `json:"scopeKind"`
	ScopeID   string    `json:"scopeId"`
	Dimension Dimension `json:"dimension"`
	// Limit is in the dimension's own unit: tokens, or millionths of a quota
	// unit for Units. Keeping it that way rather than normalising everything to
	// one currency is what lets an operator state a budget in the terms they
	// actually think in.
	Limit int64 `json:"limit"`
	// Window is how far back the rule looks. Rolling, not anchored: the last
	// five hours means the last five hours, whenever the request arrives.
	Window time.Duration `json:"-"`
}

// WindowSeconds is the window in seconds.
//
// Not a plain encoding of Window: a time.Duration marshals as nanoseconds, and
// an operator reading 2592000000000 has to know it is nanoseconds to be
// believed. Seconds is what the window was written as and what a reader
// compares against.
func (r Rule) WindowSeconds() int64 { return int64(r.Window / time.Second) }

// Resolution is the bucket size this rule is enforced at. Derived from the
// window and stored alongside it, so a rule keeps meaning the same thing even
// if the derivation is retuned.
func (r Rule) Resolution() time.Duration { return ResolutionFor(r.Window) }

// ScopeKey is what this rule's spend is counted under.
//
// Not the request's scope. A request carries both the tenant and the project,
// and the two hold separate rules; counting them under the request's own key
// would put an envelope and its partition in one row, where they would sum into
// each other and the tenant's total would be reported as the project's.
//
// A tenant rule keys on the tenant alone, so every project under it lands in
// the same row. That is not an optimisation — it is what makes the envelope an
// envelope rather than one budget per project.
func (r Rule) ScopeKey() string { return r.ScopeID }

// Validate reports the first way this rule cannot be enforced.
func (r Rule) Validate() error {
	if !r.ScopeKind.Valid() {
		return fmt.Errorf("scope kind %q must be %q or %q", r.ScopeKind, KindTenant, KindProject)
	}
	if r.ScopeID == "" {
		return fmt.Errorf("rule has no scope id")
	}
	if !r.Dimension.Valid() {
		return fmt.Errorf("unknown dimension %q; expected one of %s",
			r.Dimension, strings.Join(DimensionNames(), ", "))
	}
	if r.Limit <= 0 {
		// Zero would mean "refuse everything", which is never what an operator
		// writing a rule of zero intends and is spelled by deleting the rule.
		return fmt.Errorf("limit must be positive; delete the rule to remove the cap")
	}
	if r.Window <= 0 {
		return fmt.Errorf("window must be positive")
	}
	if r.Window > 366*24*time.Hour {
		return fmt.Errorf("window %s is longer than a year", r.Window)
	}
	return nil
}

// ScopeKind says which table a rule's scope id refers to.
type ScopeKind string

const (
	KindTenant  ScopeKind = "tenant"
	KindProject ScopeKind = "project"
)

func (k ScopeKind) Valid() bool { return k == KindTenant || k == KindProject }

// DimensionNames lists the dimensions, for error messages and the console.
func DimensionNames() []string {
	out := make([]string, len(Dimensions))
	for i, d := range Dimensions {
		out[i] = string(d)
	}
	return out
}

// Request is one request's upper bound, per dimension.
//
// Every dimension is reserved, not just the one the caller cares about, because
// a rule may be added at any time and a rule added after the fact must be able
// to see what the traffic did — a counter that only accumulated the dimensions
// someone thought of last week has no history to enforce against.
type Request struct {
	Scope ratelimit.Scope
	// Estimate is the upper bound: the prompt as the gateway counted it plus
	// the client's ceiling, priced. It must be an upper bound, not a
	// prediction, because the point of reserving before forwarding is that the
	// real cost is not known yet.
	Estimate Estimate
	Now      time.Time
}

// Estimate is what a request could cost, in every dimension at once.
type Estimate struct {
	Usage  openai.Usage
	Amount billing.Amount
	// Known is false when the model had no price and the figures are a floor
	// rate rather than this model's own. The caller decides what that means;
	// see the gateway's reserve path.
	Known bool
}

// Measure returns this dimension's value from the estimate.
func (e Estimate) Measure(d Dimension) int64 { return d.Measure(e.Usage, e.Amount) }

// Reservation is a set of committed deductions awaiting settlement.
//
// Opaque and passed straight back, as in ratelimit: the caller holds exactly
// one and the shared state stays on the far side of the interface.
//
// It remembers the rules rather than opaque keys because settlement has to
// re-derive which column each deduction belongs in, and a key that encoded the
// column would have to be parsed back to find out — the rule is the thing that
// already holds that answer.
