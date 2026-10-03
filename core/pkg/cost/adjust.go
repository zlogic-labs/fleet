package cost

import (
	"sort"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Corrections between periods.
//
// There is no balance sheet in Fleet, and that is a decision rather than an
// omission. A period's cost is an apportionment of a pool that has already been
// paid for, not a charge drawn against a prepaid account, so there is nothing
// for a tenant to owe forward. And a request is reserved and accounted for in
// the same breath (P5), so no request is admitted in one period and paid in
// another — a tenant cannot run up a debt simply by sending a lot of traffic,
// because the traffic that exceeds their budget is refused rather than served
// and charged.
//
// What Fleet does have is the other kind of problem: a closed period is
// immutable, and the observations behind it are not complete when it closes. A
// usage event written after midnight for a request that finished at 23:59, a
// capacity sample the operator had not reported yet, a reconcile that replaces
// an estimated count with the engine's real one. All of them belong to a period
// that can no longer change.
//
// So the rule is the accounting one: the invoice as issued never moves, and a
// correction discovered later is booked as an adjustment in the period that
// discovered it. Nothing is rewritten and nothing disappears.

// Adjustment is the movement one scope owes the current period because an
// earlier period was recomputed.
type Adjustment struct {
	// Scope is a tenant, in the same form the allocation rows use.
	Scope string `json:"scope"`
	// ForPeriod is the closed period being corrected.
	ForPeriod string `json:"forPeriod"`
	// GPUSeconds moves with the amount so a console can show what changed in
	// the physical terms the pool is denominated in, not only in money.
	GPUSeconds int64          `json:"gpuSeconds"`
	Amount     billing.Amount `json:"amount"`
}

// Amendment is one scope's movement, seen from the period that was corrected.
//
// It is the same Adjustment read by for_period rather than by period: the trail
// has to be legible from both ends, because the operator looking at a disputed
// month has no way to know which month later found the problem.
type Amendment = Adjustment

// Diff returns what a recomputation changed, per scope.
//
// Compared on the allocated amount rather than on shares, because shares are
// ratios: a period revised because one late event arrived moves one tenant's
// amount and every other tenant's share, while the money moved once. Billing the
// difference in shares would re-charge the whole fleet for one missing row.
//
// Scopes that disappeared entirely report a negative movement rather than
// vanishing, so a correction can reduce a bill as well as raise one.
func Diff(previous, revised Report) []Adjustment {
	byScope := make(map[string]*Adjustment, len(revised.Tenants))
	for _, t := range revised.Tenants {
		byScope[t.Key] = &Adjustment{
			Scope:      t.Key,
			ForPeriod:  revised.Period,
			GPUSeconds: t.GPUSeconds,
			Amount:     t.Amount,
		}
	}
	for _, t := range previous.Tenants {
		cur, ok := byScope[t.Key]
		if !ok {
			byScope[t.Key] = &Adjustment{
				Scope:      t.Key,
				ForPeriod:  revised.Period,
				GPUSeconds: -t.GPUSeconds,
				Amount:     -t.Amount,
			}
			continue
		}
		cur.Amount -= t.Amount
		cur.GPUSeconds -= t.GPUSeconds
	}

	out := make([]Adjustment, 0, len(byScope))
	for _, adj := range byScope {
		if adj.Amount == 0 && adj.GPUSeconds == 0 {
			continue
		}
		out = append(out, *adj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

// apply folds adjustments into the allocation rows.
//
// Folded rather than reported beside them so that reading the table still adds
// up to Allocated — the one property an operator checks first, and the one a
// report that keeps corrections in a separate list breaks. A scope that owes a
// correction but consumed nothing in this period still gets a row: that is the
// whole point of the correction, and dropping it would lose the money the moment
// a tenant stopped sending traffic.
//
// GPU-seconds are left alone. A row's consumption is what this fleet did in this
// month; a correction's GPUSeconds belongs to another month, and adding it here
// would make the capacity column disagree with the utilisation report above it,
// which computes from the same sweep. The movement is reported in Adjustments
// and Amended, where the month it belongs to is named.
func apply(rows []Tenant, adjustments []Adjustment) ([]Tenant, billing.Amount) {
	byScope := make(map[string]int, len(rows))
	for i, t := range rows {
		byScope[t.Key] = i
	}
	total := billing.Amount(0)
	for _, t := range rows {
		total += t.Amount
	}

	for _, adj := range adjustments {
		i, ok := byScope[adj.Scope]
		if !ok {
			rows = append(rows, Tenant{
				Key:        adj.Scope,
				Amount:     adj.Amount,
				Adjustment: adj.Amount,
			})
			byScope[adj.Scope] = len(rows) - 1
			total += adj.Amount
			continue
		}
		rows[i].Amount += adj.Amount
		rows[i].Adjustment += adj.Amount
		total += adj.Amount
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows, total
}
