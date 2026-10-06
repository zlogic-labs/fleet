package cost

import (
	"fmt"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// What a close is handed.

// Input is everything a close needs. Nothing here is read lazily, so a close is
// a pure function of its inputs and the same input always produces the same
// report — which is what makes the result checkable afterwards.
type Input struct {
	Period Period
	Rates  []Rate
	// Capacity is GPUs present, per cluster, over time. It is the pool.
	Capacity map[string][]Sample
	// Reserved is per-deployment GPU capacity over time — the pool a deployment
	// is holding open, paid for whether or not anybody asks.
	Reserved map[string][]Sample
	// Spans is per-deployment request intervals. Used and Consumption are
	// derived from these by Sweep rather than summed from them: adding up each
	// request's wall clock bills a GPU once per request per second, so a month
	// under load costs more than the same month idle, and a tenant's bill goes
	// down if they serialise their traffic.
	Spans map[string][]Span
	// Spent is what the ledger charged each key. It is reported beside each
	// tenant's share because the gap between the two is the price book's
	// health: a tenant billed well above what the fleet cost is over-priced,
	// and the platform can see that nowhere else.
	Spent map[string]billing.Amount
	// Direct is what commercial providers charged per key during the period,
	// keyed like Spent.
	//
	// Separate from Spent rather than merged into it, and the separation is the
	// whole point. A fleet rate is a weighting that converts GPU time into a
	// share of a fixed pool; a vendor rate is money per token. They answer
	// different questions, so summing them would produce a number that is
	// neither a pool share nor an invoice.
	Direct map[string]billing.Amount
	// Providers is what each commercial provider charged, for the period total
	// and for reconciling against the vendor's own invoice. The field a
	// deployment needs and cannot derive: Fleet knows what it reserved, and the
	// vendor's statement is what actually arrived.
	//
	// Per provider rather than summed, and carrying a request count, because the
	// reconciliation question is "does this match the bill" — which is asked of
	// one vendor at a time and is checkable per request.
	Providers map[string]ProviderCharge
	// MinCoverage is the fraction of the period the samples must account for.
	MinCoverage float64
	// Adjustments are corrections booked into this period by the revision of an
	// earlier one. Read before the close so they are inside the total rather
	// than bolted on afterwards.
	Adjustments []Adjustment
	// Amended names the movements this computation causes, recorded on the report
	// so the direction of the trail survives the details being aggregated.
	Amended []Amendment
	// Revision is the revision being written. Zero means the first.
	Revision int
}

// ProviderCharge is what one commercial provider charged over a period.
type ProviderCharge struct {
	Amount billing.Amount
	// Requests counts the ledger rows, not distinct conversations. It is here
	// so a mismatch against a vendor statement can be narrowed to "we sent more
	// requests" versus "the per-request price moved", which are different
	// problems.
	Requests int64
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
