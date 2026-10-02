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
	Share      int64          `json:"share"`
	GPUSeconds int64          `json:"gpuSeconds"`
	Amount     billing.Amount `json:"amount"`
	UsageMicro billing.Amount `json:"usageMicro"`
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
}

// Input is everything a close needs. Nothing here is read lazily, so a close is
// a pure function of its inputs and the same input always produces the same
// report — which is what makes the result checkable afterwards.
type Input struct {
	Period Period
	Rates  []Rate
	// Capacity is GPUs present, per cluster, over time. It is the pool.
	Capacity map[string][]Sample
	// Reserved is per-deployment GPU capacity over time, and Used is per
	// deployment GPU-seconds consumed.
	Reserved map[string][]Sample
	Used     map[string]int64
	// Consumption is per-key GPU-seconds, for the allocation, and Spent is what
	// the ledger charged those same keys. Both are reported per row because the
	// gap between them is the price book's health: a tenant billed well above
	// what the fleet cost is over-priced, and the platform cannot see that
	// anywhere else.
	Consumption []Use
	Spent       map[string]billing.Amount
	// MinCoverage is the fraction of the period the samples must account for.
	MinCoverage float64
}

// DefaultMinCoverage is 90%: a month that is only 60% observed is not a month,
// and reporting it as one produces an invoice nobody can reconcile.
const DefaultMinCoverage = 0.9

// ErrIncomplete reports a period whose samples do not cover enough of it.
type ErrIncomplete struct {
	Period   string
	Coverage float64
	Want     float64
}

func (e *ErrIncomplete) Error() string {
	return fmt.Sprintf("cost: period %s is only %.0f%% observed, need %.0f%% to close it",
		e.Period, e.Coverage*100, e.Want*100)
}

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
		Period: in.Period.String(),
		From:   in.Period.Start,
		To:     in.Period.End,
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

	rep.Deployments = perDeployment(in)
	rep.Tenants, rep.Allocated = allocate(rep.Pool, in.Consumption, in.Spent)
	rep.Busy = proportion(rep.Pool, consumed(in), poolSeconds)
	rep.Idle = rep.Pool - rep.Busy
	rep.IdlePct = percent(int64(rep.Idle), int64(rep.Pool))
	return rep, nil
}

func rateIndex(rates []Rate) map[string]Rate {
	byCluster := make(map[string]Rate, len(rates))
	for _, r := range rates {
		byCluster[r.Cluster] = r
	}
	return byCluster
}

// priceSeconds converts GPU-seconds at a per-GPU-hour rate.
//
// The rate is already in micro-units, so the divisor is just an hour. Scaling by
// MicroPerUnit here as well prices a month at a millionth of what it cost, which
// is a plausible-looking number and wildly wrong.
func priceSeconds(seconds, rateMicro int64) billing.Amount {
	const gpuSecondsPerHour = 3600
	num := seconds * rateMicro
	return billing.Amount((num + gpuSecondsPerHour/2) / gpuSecondsPerHour)
}

// proportion is part of pool, by part of whole, clamped to pool.
func proportion(pool billing.Amount, part, whole int64) billing.Amount {
	if pool <= 0 || whole <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return pool
	}
	return billing.Amount(int64(pool) * part / whole)
}

func consumed(in Input) int64 {
	var total int64
	for _, u := range in.Consumption {
		total += u.GPUSeconds
	}
	return total
}

// percent is part/whole as a whole-number percentage, rounded half up.
//
// Half up rather than truncating because IdlePct is read as "78% idle", and
// truncation calls a fleet that is 78.6% idle 78%, which flatters it.
func percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	if part < 0 {
		return -percent(-part, whole)
	}
	const hundred = 100
	return int((part*hundred*2 + whole) / (whole * 2))
}
