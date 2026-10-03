package cost

import (
	"sort"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// allocate splits the pool across keys in proportion to GPU-seconds consumed.
//
// Largest-remainder rounding, so the parts sum to exactly the pool. Giving the
// remainder to whoever happened to be last would let a tenant's bill change
// because an unrelated tenant was added, and "the invoices do not add up" is
// the fastest way to lose a customer's trust in a billing report.
func allocate(pool billing.Amount, uses []Use, spent map[string]billing.Amount) ([]Tenant, billing.Amount) {
	if len(uses) == 0 {
		return nil, 0
	}
	rows := make([]Tenant, 0, len(uses))
	var total int64
	for _, u := range uses {
		if u.GPUSeconds <= 0 {
			continue
		}
		total += u.GPUSeconds
	}
	if total <= 0 {
		return nil, 0
	}

	type remainder struct {
		idx  int
		frac int64
	}
	var assigned int64
	var fracs []remainder

	for _, u := range uses {
		if u.GPUSeconds <= 0 {
			continue
		}
		num := u.GPUSeconds * int64(pool)
		amount := num / total
		assigned += amount
		fracs = append(fracs, remainder{len(rows), num % total})
		rows = append(rows, Tenant{
			Key:        u.Key,
			GPUSeconds: u.GPUSeconds,
			Share:      u.GPUSeconds * 1_000_000 / total,
			Amount:     billing.Amount(amount),
			UsageMicro: spent[u.Key],
		})
	}

	left := int64(pool) - assigned
	if left > 0 {
		sort.SliceStable(fracs, func(i, j int) bool { return fracs[i].frac > fracs[j].frac })
		for i := 0; i < int(left) && i < len(fracs); i++ {
			rows[fracs[i].idx].Amount++
		}
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows, billing.Amount(assigned + left)
}

// perDeployment reports reserved, used and idle GPU-seconds per deployment.
//
// Seconds only, no money: a rate is declared per cluster and a deployment is
// against, so a negative here would mean the bound broke, and printing zero
// over it would hide that.
func perDeployment(in Input, used map[string]int64) []Deployment {
	names := deploymentNames(in)

	rows := make([]Deployment, 0, len(names))
	for _, name := range names {
		reserved, _ := Integrate(in.Reserved[name], in.Period.Start, in.Period.End)
		u := used[name]
		idle := reserved - u
		rows = append(rows, Deployment{
			Name:     name,
			Reserved: reserved,
			Used:     u,
			Idle:     idle,
			IdlePct:  percent(idle, reserved),
		})
	}
	return rows
}

// deploymentNames is every deployment with capacity or with traffic, sorted.
func deploymentNames(in Input) []string {
	names := make([]string, 0, len(in.Reserved))
	for name := range in.Reserved {
		names = append(names, name)
	}
	for name := range in.Spans {
		if _, ok := in.Reserved[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
