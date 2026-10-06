package cost

import (
	"sort"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Direct spend: money that left the account per request, as against a share of
// a pool that was paid for either way.
//
// The functions here exist so that two figures which cannot be added are never
// added on the way into a report. A tenant row carries both, a provider row
// carries one, and Report.Allocated is untouched by anything a vendor charged.

// withDirect attaches vendor charges to the allocation rows.
//
// Rows are added for keys that only have direct spend. That case is not exotic:
// a tenant that bought everything it used has no GPU-seconds, so allocate
// produced no row for it, and dropping it would make a report that said they
// spent nothing. The added row has no share and no GPU-seconds, which is the
// honest description of a tenant who consumed none of the fleet's capacity.
//
// Existing rows are matched by key rather than by index, because allocate sorts
// its output and a positional merge would attach a tenant's vendor bill to
// whichever tenant happened to land on the same position.
func withDirect(rows []Tenant, direct map[string]billing.Amount) []Tenant {
	if len(direct) == 0 {
		return rows
	}
	out := make([]Tenant, len(rows))
	copy(out, rows)
	seen := make(map[string]int, len(out))
	for i, row := range out {
		seen[row.Key] = i
	}

	// Sorted so the same inputs produce the same rows in the same order, which
	// is the property that makes a report reproducible from the ledger alone.
	keys := make([]string, 0, len(direct))
	for key, amount := range direct {
		if amount == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		amount := direct[key]
		if i, ok := seen[key]; ok {
			out[i].Direct = amount
			continue
		}
		out = append(out, Tenant{Key: key, Direct: amount})
		seen[key] = len(out) - 1
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// providerSpend turns the per-provider totals into report rows, sorted by name.
//
// Sorted rather than ordered by amount: the answer to "which vendor did we pay"
// should not depend on map order, and two closes of the same data have to
// produce byte-identical output for a stored invoice to be comparable.
func providerSpend(byProvider map[string]ProviderCharge) []ProviderSpend {
	if len(byProvider) == 0 {
		return nil
	}
	out := make([]ProviderSpend, 0, len(byProvider))
	for provider, charge := range byProvider {
		if provider == "" || charge.Amount == 0 {
			// Empty is the fleet, and the fleet is not a vendor. It is priced
			// by the pool above; listing it here would invite the reader to add
			// the two, which is exactly the arithmetic this file exists to
			// prevent.
			continue
		}
		out = append(out, ProviderSpend{
			Provider: provider,
			Amount:   charge.Amount,
			Requests: charge.Requests,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// totalDirect is the sum of the vendor rows.
func totalDirect(rows []ProviderSpend) billing.Amount {
	var sum billing.Amount
	for _, row := range rows {
		sum += row.Amount
	}
	return sum
}
