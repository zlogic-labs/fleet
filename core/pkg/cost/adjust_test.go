package cost

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Corrections between periods, tested without a database: the rules are about
// arithmetic, and the arithmetic is where a billing mistake hides.

func reportWith(period string, tenants ...Tenant) Report {
	return Report{Period: period, Tenants: tenants}
}

func tenant(key string, seconds, amount int64) Tenant {
	return Tenant{Key: key, GPUSeconds: seconds, Amount: billing.Amount(amount)}
}

func TestDiffMovesOnlyTheScopeThatChanged(t *testing.T) {
	previous := reportWith("2026-09",
		tenant("acme", 600, 600_000),
		tenant("globex", 400, 400_000))
	revised := reportWith("2026-09",
		tenant("acme", 900, 900_000),
		tenant("globex", 400, 400_000))

	deltas := Diff(previous, revised)
	if len(deltas) != 1 {
		t.Fatalf("got %d adjustments, want 1: %+v", len(deltas), deltas)
	}
	if deltas[0].Scope != "acme" || deltas[0].Amount != 300_000 || deltas[0].GPUSeconds != 300 {
		t.Fatalf("adjustment %+v", deltas[0])
	}
	if deltas[0].ForPeriod != "2026-09" {
		t.Fatalf("for period %q", deltas[0].ForPeriod)
	}
}

func TestDiffReportsAScopeThatIsNoLongerThere(t *testing.T) {
	previous := reportWith("2026-09", tenant("acme", 600, 600_000), tenant("gone", 100, 100_000))
	revised := reportWith("2026-09", tenant("acme", 600, 600_000))

	deltas := Diff(previous, revised)
	if len(deltas) != 1 || deltas[0].Scope != "gone" {
		t.Fatalf("adjustments %+v", deltas)
	}
	// Negative, because a correction can refund as well as charge. Printing an
	// absolute value here would invoice the tenant for a row that is being
	// removed.
	if deltas[0].Amount != -100_000 || deltas[0].GPUSeconds != -100 {
		t.Fatalf("adjustment %+v", deltas[0])
	}
}

func TestAnUnchangedRecomputationMovesNothing(t *testing.T) {
	rep := reportWith("2026-09", tenant("acme", 600, 600_000))
	if deltas := Diff(rep, rep); len(deltas) != 0 {
		t.Fatalf("got %+v, want no adjustment", deltas)
	}
}

func TestACorrectionReachesAScopeThatConsumedNothing(t *testing.T) {
	rows, total := apply(nil, []Adjustment{{Scope: "acme", ForPeriod: "2026-09", Amount: 120_000}})

	if len(rows) != 1 || rows[0].Key != "acme" {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Amount != 120_000 || rows[0].Adjustment != 120_000 {
		t.Fatalf("row %+v", rows[0])
	}
	if total != 120_000 {
		t.Fatalf("total %d", total)
	}
}

func TestACorrectionIsAddedToAnExistingShare(t *testing.T) {
	rows, total := apply(
		[]Tenant{tenant("acme", 600, 600_000)},
		[]Adjustment{{Scope: "acme", ForPeriod: "2026-09", Amount: -20_000, GPUSeconds: -300}})

	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Amount != 580_000 || rows[0].Adjustment != -20_000 {
		t.Fatalf("row %+v", rows[0])
	}
	if total != 580_000 {
		t.Fatalf("total %d", total)
	}
	// The GPU-seconds belong to September. Carrying them into this month's row
	// would make the allocation table's capacity column disagree with the
	// utilisation figures computed from the same sweep, and a tenant's share
	// would no longer be the share they are being billed.
	if rows[0].GPUSeconds != 600 {
		t.Fatalf("a correction changed this month's consumption: %d", rows[0].GPUSeconds)
	}
}

func TestTheRowsStillAddUpAfterACorrection(t *testing.T) {
	// The property an operator checks first: reading the table gives the same
	// number as the total. A report that keeps corrections in a side list
	// breaks it the moment one appears.
	rows, total := apply(
		[]Tenant{tenant("acme", 600, 600_000), tenant("globex", 400, 400_000)},
		[]Adjustment{{Scope: "acme", Amount: 50_000}, {Scope: "new", Amount: -7_000}})

	var sum billing.Amount
	for _, r := range rows {
		sum += r.Amount
	}
	if sum != total {
		t.Fatalf("rows sum to %d, total says %d", sum, total)
	}
	if total != 1_043_000 {
		t.Fatalf("total %d", total)
	}
	if rows[0].Key != "acme" || rows[2].Key != "new" {
		t.Fatalf("rows are not sorted by scope: %+v", rows)
	}
}

func TestCloseExplainsWhyAllocatedIsNotPool(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000}}
	in.Spans = map[string][]Span{"llama": held("acme", januaryHours)}
	in.Adjustments = []Adjustment{
		{Scope: "acme", ForPeriod: "2025-12", Amount: 500_000},
	}

	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.Allocated != rep.Pool+billing.Amount(500_000) {
		t.Fatalf("allocated %s, pool %s, adjustment %s", rep.Allocated, rep.Pool, rep.AdjustmentTotal)
	}
	if rep.AdjustmentTotal != 500_000 {
		t.Fatalf("adjustment total %d", rep.AdjustmentTotal)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("no note explaining an allocated figure that is not the pool")
	}
	var sum billing.Amount
	for _, r := range rep.Tenants {
		sum += r.Amount
	}
	if sum != rep.Allocated {
		t.Fatalf("rows sum to %s, allocated is %s", sum, rep.Allocated)
	}
}

func TestAPeriodWithNoCorrectionsAllocatesExactlyThePool(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000}}
	in.Spans = map[string][]Span{"llama": held("acme", januaryHours)}

	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.Allocated != rep.Pool {
		t.Fatalf("allocated %s against a pool of %s", rep.Allocated, rep.Pool)
	}
	if len(rep.Notes) != 0 {
		t.Fatalf("notes %v", rep.Notes)
	}
}
