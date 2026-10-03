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
