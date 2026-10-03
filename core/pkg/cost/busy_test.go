package cost

import (
	"testing"
	"time"
)

// The sweep is where "how much did this cost" stops being arithmetic and starts
// being a claim about concurrency, so most of these are the claims we would be
// embarrassed to get wrong on an invoice.

var sweepStart = at("2026-03-01T00:00:00Z")

func atSec(s int) time.Time { return sweepStart.Add(time.Duration(s) * time.Second) }

// oneGPU is a deployment holding a single GPU open for the whole window.
func oneGPU() []Sample { return []Sample{{At: sweepStart, Value: 1}} }

// concurrent builds n requests by one key, all covering the same interval.
func concurrent(key string, gpus int64, from, to time.Time, n int) []Span {
	out := make([]Span, n)
	for i := range out {
		out[i] = Span{From: from, To: to, Key: key, GPUs: gpus}
	}
	return out
}

func TestSweepBillsConcurrencyOnceNotOncePerRequest(t *testing.T) {
	// The bug this whole file exists for. Ten requests sharing one GPU for ten
	// seconds is ten GPU-seconds; summing wall clocks calls it a hundred, which
	// makes an idle fleet look busier than a saturated one and hands a tenant a
	// discount for serialising their traffic.
	b := Sweep(oneGPU(), concurrent("acme", 1, atSec(0), atSec(10), 10), sweepStart, atSec(100))
	if b.Seconds != 10 {
		t.Fatalf("billed %d GPU-seconds for 10 seconds on 1 GPU, want 10", b.Seconds)
	}
}

func TestSweepCannotOccupyMoreGPUsThanExist(t *testing.T) {
	// Two keys each asking for four GPUs on a two-GPU deployment still get two
	// GPU-seconds a second between them, not four.
	spans := append(
		concurrent("acme", 4, atSec(0), atSec(10), 3),
		concurrent("zeta", 4, atSec(0), atSec(10), 3)...)
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.Seconds != 10 {
		t.Fatalf("used %d GPU-seconds on a 1-GPU deployment for 10 seconds, want 10", b.Seconds)
	}
}

func TestSweepSplitsABusyInstantByDemand(t *testing.T) {
	// Six requests from acme and three from zeta, all at once: acme had two
	// thirds of the traffic and gets two thirds of the GPU-second, to the
	// nearest whole second.
	spans := append(
		concurrent("acme", 1, atSec(0), atSec(10), 6),
		concurrent("zeta", 1, atSec(0), atSec(10), 3)...)
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 7 || b.ByKey["zeta"] != 3 {
		t.Fatalf("acme %d zeta %d, want 7 and 3", b.ByKey["acme"], b.ByKey["zeta"])
	}
}

func TestSweepCountsSequentialRequestsSeparately(t *testing.T) {
	// Acme holds a one-GPU deployment for 10 seconds and zeta for 10 more. zeta
	// is nobody's neighbour, so it does not have to wait for acme to leave.
	spans := []Span{
		{From: atSec(0), To: atSec(10), Key: "acme", GPUs: 1},
		{From: atSec(10), To: atSec(20), Key: "zeta", GPUs: 1},
	}
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 10 || b.ByKey["zeta"] != 10 || b.Seconds != 20 {
		t.Fatalf("acme %d zeta %d total %d, want 10, 10, 20", b.ByKey["acme"], b.ByKey["zeta"], b.Seconds)
	}
}

func TestSweepSharesAreOfTheBusyTimeNotOfTheDemand(t *testing.T) {
	// zeta's demand never changes — one request, one GPU, from t=0 to t=20 —
	// and its share still goes from half the busy time to all of it when acme
	// leaves. A rate tracked per key would have frozen zeta at a half.
	spans := []Span{
		{From: atSec(0), To: atSec(10), Key: "acme", GPUs: 1},
		{From: atSec(0), To: atSec(20), Key: "zeta", GPUs: 1},
	}
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 5 || b.ByKey["zeta"] != 15 {
		t.Fatalf("acme %d zeta %d, want 5 and 15", b.ByKey["acme"], b.ByKey["zeta"])
	}
}

func TestSweepKeysAlwaysSumToTheWhole(t *testing.T) {
	// Seconds is defined as the sum, so a report can never claim more than its
	// rows add up to — which is the first thing anybody checks.
	spans := append(
		concurrent("acme", 1, atSec(0), atSec(37), 5),
		concurrent("zeta", 1, atSec(3), atSec(41), 11)...)
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	var sum int64
	for _, v := range b.ByKey {
		sum += v
	}
	if sum != b.Seconds {
		t.Fatalf("keys sum to %d but Seconds is %d", sum, b.Seconds)
	}
	if b.Seconds <= 0 {
		t.Fatal("a deployment with requests in it reported no usage")
	}
}

func TestSweepOfADeploymentScaledToZeroIsIdle(t *testing.T) {
	// Capacity said zero, traffic says otherwise. The capacity sample is the
	// operator's statement about the hardware, and it wins.
	zero := []Sample{{At: sweepStart, Value: 0}}
	if b := Sweep(zero, concurrent("acme", 8, atSec(0), atSec(10), 3), sweepStart, atSec(100)); b.Seconds != 0 {
		t.Fatalf("a deployment with no GPUs reported %d GPU-seconds", b.Seconds)
	}
}

func TestSweepWithNoCapacitySampleTakesDemandAtFaceValue(t *testing.T) {
	// The shape is simply unknown. Inventing a smaller pool would report a
	// fleet as idle when it was serving traffic, which is the one answer a cost
	// report must never give.
	spans := concurrent("acme", 4, atSec(0), atSec(10), 2)
	b := Sweep(nil, spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 80 {
		t.Fatalf("acme %d GPU-seconds, want 80 (two requests asking four GPUs each for ten seconds)", b.ByKey["acme"])
	}
}

func TestSweepClampsRequestsToTheWindow(t *testing.T) {
	// A month closes over a month. A request that started the previous month is
	// charged only for the part inside it, and one that ends next month is
	// charged only up to the boundary.
	spans := []Span{
		{From: atSec(-100), To: atSec(10), Key: "acme", GPUs: 1},
		{From: atSec(90), To: atSec(500), Key: "zeta", GPUs: 1},
	}
	b := Sweep(oneGPU(), spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 10 || b.ByKey["zeta"] != 10 {
		t.Fatalf("acme %d zeta %d, want 10 and 10", b.ByKey["acme"], b.ByKey["zeta"])
	}
}

func TestSweepFollowsCapacityAsItSteps(t *testing.T) {
	// A deployment scaled from 1 GPU to 4 at t=50. Both halves can only keep as
	// many requests busy as they have GPUs: three requests occupy one GPU for
	// fifty seconds and three GPUs for the next fifty.
	capacity := []Sample{{At: atSec(0), Value: 1}, {At: atSec(50), Value: 4}}
	spans := concurrent("acme", 1, atSec(0), atSec(100), 3)
	b := Sweep(capacity, spans, sweepStart, atSec(100))
	if b.Seconds != 50*1+50*3 {
		t.Fatalf("used %d GPU-seconds, want 200 (50 at one GPU, then 150 at three)", b.Seconds)
	}
}

func TestSweepSubSecondEdgesDoNotVanish(t *testing.T) {
	// A month has about 2.6 million seconds and a busy deployment can put
	// thousands of edges inside one of them. Truncating each contribution to
	// whole GPU-seconds would leave four thousand edges adding up to nothing.
	capacity := []Sample{{At: sweepStart, Value: 1}}
	var spans []Span
	for i := range 4000 {
		spans = append(spans, Span{
			From: sweepStart.Add(time.Duration(i) * time.Millisecond),
			To:   sweepStart.Add(time.Duration(i+1) * time.Millisecond),
			Key:  "acme",
			GPUs: 1,
		})
	}
	b := Sweep(capacity, spans, sweepStart, atSec(10))
	if b.ByKey["acme"] != 4 {
		t.Fatalf("4000 one-millisecond requests totalled %d GPU-seconds, want 4", b.ByKey["acme"])
	}
}

func TestSweepRescalesEveryKeyWhenTheDeploymentFreesUp(t *testing.T) {
	// Three keys on three GPUs, then acme leaves. While all three are in flight
	// each occupies a GPU of its own, so each gets a GPU-second a second; once
	// acme goes the other two still each hold exactly one of the three. A rate
	// tracked per key would leave them claiming more than the deployment has.
	capacity := []Sample{{At: sweepStart, Value: 3}}
	spans := []Span{
		{From: atSec(0), To: atSec(10), Key: "acme", GPUs: 1},
		{From: atSec(0), To: atSec(30), Key: "zeta", GPUs: 1},
		{From: atSec(0), To: atSec(30), Key: "eta", GPUs: 1},
	}
	b := Sweep(capacity, spans, sweepStart, atSec(100))
	if b.ByKey["acme"] != 10 || b.ByKey["zeta"] != 30 || b.ByKey["eta"] != 30 {
		t.Fatalf("acme %d zeta %d eta %d, want 10, 30, 30", b.ByKey["acme"], b.ByKey["zeta"], b.ByKey["eta"])
	}
}

func TestSweepStopsTrustingASampleAfterTheSameGapIntegrateDoes(t *testing.T) {
	// One sample, then silence. Integrate stops counting a day later, so the
	// sweep must stop too — otherwise the deployment reports a month of usage
	// against a day of reserved capacity and its idle time goes negative.
	capacity := []Sample{{At: sweepStart, Value: 1}}
	start := sweepStart
	b := Sweep(capacity, []Span{{
		From: start.Add(MaxGap / 2),
		To:   start.Add(MaxGap + time.Hour),
		Key:  "acme",
		GPUs: 1,
	}}, start, start.Add(MaxGap+2*time.Hour))

	if b.ByKey["acme"] != int64(MaxGap/time.Second/2) {
		t.Fatalf("used %d GPU-seconds, want only the %d before the sample expired",
			b.ByKey["acme"], MaxGap/time.Second/2)
	}
}
