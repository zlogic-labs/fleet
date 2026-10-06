package cost

import (
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Rate is what one GPU-hour costs the operator.
//
// Fleet cannot know this. A cloud bill, a colocation contract and a
// depreciated on-prem fleet have nothing in common, so the number is declared
// per cluster rather than derived. Declaring it wrong makes every invoice wrong,
// which is why an unset rate produces an explicitly unpriced report instead of
// a zero one.
type Rate struct {
	Cluster string `json:"cluster"`
	// GPUHourMicro is billing.Amount per GPU-hour: millionths of a currency unit.
	GPUHourMicro int64  `json:"gpuHourMicro"`
	Currency     string `json:"currency"`
}

// Use is GPU-seconds consumed by one key during the period.
//
// Key is a tenant, a project or a deployment — the report groups by whatever
// the caller asked for, the same way postgres.Spend does.
type Use struct {
	Key        string `json:"key"`
	GPUSeconds int64  `json:"gpuSeconds"`
}

// Tenant is one row of the allocation.
type Tenant struct {
	Key string `json:"key"`
	// Share is GPUSeconds over all GPUSeconds, in millionths, so a console can
	// render a percentage without a float rounding differently from the server.
	Share      int64 `json:"share"`
	GPUSeconds int64 `json:"gpuSeconds"`
	// Amount is this tenant's share of the fleet's own pool. It is not what
	// they owe: see Direct.
	Amount billing.Amount `json:"amount"`
	// Adjustment is the part of Amount that came from a correction to an earlier
	// period rather than from this period's pool. Reported separately so a
	// tenant can see why this month's figure does not match their share.
	Adjustment billing.Amount `json:"adjustment"`
	// UsageMicro is what the ledger charged this key, which is a weighting, not
	// a price.
	UsageMicro billing.Amount `json:"usageMicro"`
	// Direct is what commercial providers charged this key for the period. Real
	// money, already spent, and not a share of anything.
	//
	// A tenant with Direct but no Amount spent real money and used none of the
	// operator's GPUs — which is a perfectly ordinary way to use a gateway that
	// has both, and is why the two are separate columns rather than one total.
	Direct billing.Amount `json:"direct"`
}

// ProviderSpend is one commercial provider's charges for a period.
//
// Kept per provider rather than summed, because this is the figure an operator
// reconciles against the vendor's own invoice. A total would answer "did we
// over-spend" only if you already knew how to split it back out.
type ProviderSpend struct {
	Provider string         `json:"provider"`
	Amount   billing.Amount `json:"amount"`
	Requests int64          `json:"requests"`
}

// Deployment is one row of the utilisation report.
type Deployment struct {
	Name     string `json:"name"`
	Reserved int64  `json:"reservedGpuSeconds"`
	Used     int64  `json:"usedGpuSeconds"`
	Idle     int64  `json:"idleGpuSeconds"`
	IdlePct  int    `json:"idlePercent"`
}

// Report is a closed period.
//
// Two views of the same money, answering different questions.
//
// Utilization: Pool is what the fleet cost, Busy is the part some request
// occupied, Idle is the difference. Idle is the number P8 exists for — a
// platform with a large Idle has a utilization problem, not a billing problem.
//
// Billing: Allocated is the whole pool, split across tenants in proportion to
// what they consumed. Allocated equals Pool, and Busy plus Idle also equals
// Pool; the two sums are different and both are true, because idle capacity is
// a fixed cost that has to land on somebody's invoice. Allocating only Busy
// would leave it unbilled and the operator would absorb the exact problem the
// platform exists to surface.
type Report struct {
	Period    string         `json:"period"`
	From      time.Time      `json:"from"`
	To        time.Time      `json:"to"`
	Currency  string         `json:"currency"`
	Priced    bool           `json:"priced"`
	Pool      billing.Amount `json:"pool"`
	Busy      billing.Amount `json:"busy"`
	Idle      billing.Amount `json:"idle"`
	IdlePct   int            `json:"idlePercent"`
	Allocated billing.Amount `json:"allocated"`
	// Revision counts how many times this period has been computed. It is 1 for
	// a period closed once, and higher only when an operator recomputed it after
	// reporting data that had been missing. Earlier revisions stay readable: an
	// invoice that changes with no trace of its previous value is not an audit
	// trail, it is an apology.
	Revision int `json:"revision"`
	// CoveragePercent is how much of the month the samples account for. A
	// report over a third of a month says so here rather than in a log line.
	CoveragePercent int `json:"coveragePercent"`
	// PoolSeconds is the capacity the fleet had, independent of price. It is
	// reported separately so an unpriced report still says how many GPU-hours
	// were available — the number that makes a rate worth discussing.
	PoolSeconds int64    `json:"poolGpuSeconds"`
	Notes       []string `json:"notes,omitempty"`

	Tenants     []Tenant     `json:"tenants"`
	Deployments []Deployment `json:"deployments"`
	// Adjustments are corrections carried in from earlier periods, and
	// AmendmentTotal is their net. When it is not zero, Allocated deliberately
	// differs from Pool and the report says so: the extra is money this period
	// is collecting on behalf of months that were already closed wrongly.
	AdjustmentTotal billing.Amount `json:"adjustmentTotal"`
	Adjustments     []Adjustment   `json:"adjustments"`
	// Amended lists the earlier periods this revision changed, so the trail is
	// readable from either end of it.
	Amended []Amendment `json:"amended"`

	// Direct is what commercial providers charged during the period, and
	// Providers breaks it down.
	//
	// Deliberately not added into Allocated. Allocated is the fleet's own pool
	// split between tenants; this is money that already left the account. A
	// single "you owe X" figure would have to average two incompatible
	// quantities, and the tenant reading it would be told their GPU share
	// absorbed vendor spend it never caused.
	Direct    billing.Amount  `json:"direct"`
	Providers []ProviderSpend `json:"providers"`
}

// Total is what this period cost the operator across both cost centres.
//
// A convenience for an operator holding the whole bill, and NOT what a tenant
// owes. It is the sum of two numbers that mean different things, so nothing
// downstream should allocate, enforce a budget against, or invoice from it —
// the two that answer those questions are Allocated and Direct.
func (r Report) Total() billing.Amount { return r.Allocated + r.Direct }

// Close turns observations into a cost report.
//
// Three steps, in this order, because each depends on the previous one:
//
//  1. integrate capacity samples per cluster and price the pool;
//  2. price each deployment's reserved and used seconds, so idle is visible per
//     deployment and not only in the total;
//  3. split the pool across tenants in proportion to what they consumed.
//
// Step 3 allocates the whole pool, not just the busy part. Charging a tenant
// only for the seconds it used would leave idle capacity — which is a fixed
// cost — with nobody to bill, and the operator would quietly absorb a
// utilization problem that the platform exists to make visible.
func Close(in Input) (Report, error) {
	if !in.Period.Valid() {
		return Report{}, fmt.Errorf("cost: period %s is not a range", in.Period.Start)
	}
	rates := rateIndex(in.Rates)

	rep := Report{
		Period:   in.Period.String(),
		From:     in.Period.Start,
		To:       in.Period.End,
		Revision: in.Revision,
	}
	if rep.Revision <= 0 {
		rep.Revision = 1
	}

	var poolSeconds int64
	covered, total := time.Duration(0), in.Period.End.Sub(in.Period.Start)
	for cluster, samples := range in.Capacity {
		seconds, cov := Integrate(samples, in.Period.Start, in.Period.End)
		poolSeconds += seconds
		covered += cov.Observed
		rate, ok := rates[cluster]
		if !ok {
			rep.Notes = append(rep.Notes, "no cost rate declared for cluster "+cluster)
			continue
		}
		rep.Priced = true
		if rep.Currency == "" {
			rep.Currency = rate.Currency
		}
		rep.Pool += priceSeconds(seconds, rate.GPUHourMicro)
	}
	rep.CoveragePercent = percent(int64(covered), int64(total))
	rep.PoolSeconds = poolSeconds
	cov := Coverage{Observed: covered, Total: total}
	if !cov.Complete(in.MinCoverage) {
		return Report{}, &ErrIncomplete{Period: rep.Period, Coverage: cov.Fraction(), Want: in.MinCoverage}
	}

	used, consumption := occupied(in)
	rep.Deployments = perDeployment(in, used)
	rep.Tenants, rep.Allocated = allocate(rep.Pool, consumption, in.Spent)
	rep.Tenants = withDirect(rep.Tenants, in.Direct)
	rep.Tenants, rep.Allocated = apply(rep.Tenants, in.Adjustments)
	rep.Providers = providerSpend(in.Providers)
	rep.Direct = totalDirect(rep.Providers)
	rep.Adjustments = in.Adjustments
	rep.Amended = in.Amended
	for _, adj := range rep.Adjustments {
		rep.AdjustmentTotal += adj.Amount
	}
	if rep.AdjustmentTotal != 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"allocated is %s of pool: %s in corrections to earlier periods are collected here",
			rep.Allocated, rep.AdjustmentTotal))
	}
	rep.Busy = proportion(rep.Pool, usedSeconds(used), poolSeconds)
	rep.Idle = rep.Pool - rep.Busy
	rep.IdlePct = percent(int64(rep.Idle), int64(rep.Pool))
	return rep, nil
}
