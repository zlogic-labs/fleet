package cost

import "testing"

// The sweep has to be a function of its input, not of how Go happened to
// walk a map, because a closed month has to read back the same way twice.

func TestSweepGivesTheSameAnswerEveryRun(t *testing.T) {
	// An exact tie on the leftover second: two keys sharing one GPU equally for
	// an odd number of seconds. Map iteration order changes between runs, so a
	// split decided by it would hand the same month out differently each time
	// somebody closed it.
	capacity := []Sample{{At: sweepStart, Value: 1}}
	spans := []Span{
		{From: atSec(0), To: atSec(1), Key: "zeta", GPUs: 1},
		{From: atSec(0), To: atSec(1), Key: "acme", GPUs: 1},
	}
	for range 200 {
		b := Sweep(capacity, spans, sweepStart, atSec(10))
		if b.ByKey["acme"] != 1 || b.ByKey["zeta"] != 0 {
			t.Fatalf("tied split came out acme %d zeta %d; the winner must not depend on map order",
				b.ByKey["acme"], b.ByKey["zeta"])
		}
	}
}

func TestSweepOfAnEmptyWindowIsNothing(t *testing.T) {
	if b := Sweep(oneGPU(), nil, sweepStart, sweepStart); b.Seconds != 0 || len(b.ByKey) != 0 {
		t.Fatalf("got %d and %v for a window that never happened", b.Seconds, b.ByKey)
	}
}

func TestSweepIgnoresZeroLengthAndZeroGPURequests(t *testing.T) {
	spans := []Span{
		{From: atSec(5), To: atSec(5), Key: "acme", GPUs: 8},
		{From: atSec(0), To: atSec(10), Key: "acme", GPUs: 0},
		{From: atSec(0), To: atSec(10), Key: "zeta", GPUs: 1},
	}
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.Seconds != 10 || b.ByKey["acme"] != 0 || b.ByKey["zeta"] != 10 {
		t.Fatalf("total %d acme %d zeta %d", b.Seconds, b.ByKey["acme"], b.ByKey["zeta"])
	}
}

func TestSweepPricesEachRequestAgainstItsOwnReplicaShape(t *testing.T) {
	// gpu_per_replica rides along on the span rather than being looked up from
	// the deployment as it is now, so a deployment rescaled mid-window cannot
	// re-price the half of the window that ran before the change.
	spans := []Span{
		{From: atSec(0), To: atSec(10), Key: "acme", GPUs: 1},
		{From: atSec(10), To: atSec(20), Key: "acme", GPUs: 4},
	}
	b := Sweep([]Sample{{At: sweepStart, Value: 8}}, spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 50 {
		t.Fatalf("used %d GPU-seconds, want 10 at one GPU then 40 at four", b.ByKey["acme"])
	}
}
