package cost

import (
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// atHour builds a sample n hours after the period start.
func atHour(h int, v int64) Sample {
	return Sample{At: jan1.Add(time.Duration(h) * time.Hour), Value: v}
}

var jan1 = at("2026-01-01T00:00:00Z")

func TestCloseRefusesAPeriodThatWasNotMostlyObserved(t *testing.T) {
	p, _ := ParsePeriod("2026-01")
	_, err := Close(Input{
		Period:      p,
		MinCoverage: DefaultMinCoverage,
		Capacity:    map[string][]Sample{"c1": {atHour(0, 8)}},
	})
	var incomplete *ErrIncomplete
	if !asErr(err, &incomplete) {
		t.Fatalf("got %v, want ErrIncomplete", err)
	}
}

func TestCloseRefusesAPeriodWithNoRateAndSaysSo(t *testing.T) {
	rep, err := Close(fullMonth())
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.Priced {
		t.Fatal("a report with no declared rate claims to be priced")
	}
	if rep.Pool != 0 || rep.Allocated != 0 {
		t.Fatalf("unpriced report claims money: pool %d allocated %d", rep.Pool, rep.Allocated)
	}
	if rep.PoolSeconds == 0 {
		t.Fatal("an unpriced report should still state how much capacity there was")
	}
}

func TestClosePricesThePoolAndTheSplit(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 3_000_000, Currency: "USD"}}
	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	// 8 GPUs for January's 744 hours at $3 per GPU-hour.
	if want := billing.Amount(8 * januaryHours * 3 * billing.MicroPerUnit); rep.Pool != want {
		t.Fatalf("pool %s, want %s", rep.Pool, billing.Amount(want))
	}
	if rep.Currency != "USD" {
		t.Fatalf("currency %q", rep.Currency)
	}
	var sum billing.Amount
	for _, tn := range rep.Tenants {
		sum += tn.Amount
	}
	if sum != rep.Allocated || sum != rep.Pool {
		t.Fatalf("tenant amounts sum to %s but allocated is %s and pool is %s", sum, rep.Allocated, rep.Pool)
	}
}

func TestCloseChargesIdleCapacityToSomebody(t *testing.T) {
	// 4 GPUs of capacity for a month, and the tenant used 100 GPU-seconds of
	// it. The pool is fixed cost (P8): it is billed out in full, and the idle
	// part shows up as utilization rather than quietly unbilled.
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 2_000_000}}
	in.Capacity = map[string][]Sample{"c1": steady(4, 1)}
	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.Allocated != rep.Pool {
		t.Fatalf("allocated %s of a %s pool: idle was left unbilled", rep.Allocated, rep.Pool)
	}
	if rep.IdlePct != 100 {
		t.Fatalf("idle %d%%, want 100%%", rep.IdlePct)
	}
	if rep.Idle != rep.Pool-rep.Busy {
		t.Fatalf("idle %s is not pool %s minus busy %s", rep.Idle, rep.Pool, rep.Busy)
	}
}

func TestCloseReportsNoIdleOnAFullyConsumedFleet(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000}}
	// Consume exactly the capacity there is.
	in.Consumption = []Use{{Key: "acme", GPUSeconds: 8 * januaryHours * 3600}}
	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.Idle != 0 || rep.IdlePct != 0 {
		t.Fatalf("idle %s (%d%%) on a fleet that was used flat out", rep.Idle, rep.IdlePct)
	}
	if rep.Busy != rep.Pool {
		t.Fatalf("busy %s of pool %s", rep.Busy, rep.Pool)
	}
}

func TestCloseReportsIdleBelowOneHundredPercent(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000}}
	in.Consumption = []Use{{Key: "acme", GPUSeconds: 8 * januaryHours * 3600 / 4}}
	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if rep.IdlePct != 75 {
		t.Fatalf("idle %d%%, want 75%%", rep.IdlePct)
	}
}

func TestCloseSplitsThePoolByConsumptionNotByKeyOrder(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000}}
	in.Consumption = []Use{{Key: "acme", GPUSeconds: 3}, {Key: "zeta", GPUSeconds: 1}}
	rep, err := Close(in)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rep.Tenants) != 2 {
		t.Fatalf("got %d tenants", len(rep.Tenants))
	}
	if rep.Tenants[0].Key != "acme" || rep.Tenants[1].Key != "zeta" {
		t.Fatalf("rows %q %q, want sorted", rep.Tenants[0].Key, rep.Tenants[1].Key)
	}
	if rep.Tenants[0].Amount <= rep.Tenants[1].Amount {
		t.Fatalf("the bigger consumer was billed %s, the smaller %s", rep.Tenants[0].Amount, rep.Tenants[1].Amount)
	}
}

func TestPerDeploymentReportsIdle(t *testing.T) {
	in := fullMonth()
	in.Reserved = map[string][]Sample{"llama": steady(1, 1)}
	in.Used = map[string]int64{"llama": 3600}
	rows := perDeployment(in)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Reserved-rows[0].Used != rows[0].Idle {
		t.Fatalf("reserved %d used %d idle %d", rows[0].Reserved, rows[0].Used, rows[0].Idle)
	}
}

func TestPerDeploymentNeverReportsNegativeIdle(t *testing.T) {
	// Overlapping requests make per-deployment used exceed any one replica's
	// reserved time. Clamping keeps the utilization report readable instead of
	// printing an idle time of minus four days.
	in := fullMonth()
	in.Reserved = map[string][]Sample{"llama": steady(1, 1)}
	in.Used = map[string]int64{"llama": januaryHours*3600 + 1}
	if idle := perDeployment(in)[0].Idle; idle != 0 {
		t.Fatalf("idle %d, want 0", idle)
	}
}
