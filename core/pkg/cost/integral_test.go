package cost

import (
	"testing"
	"time"
)

func TestIntegrateMultipliesValueByHeldTime(t *testing.T) {
	from, to := jan1, jan1.Add(3*time.Hour)
	got, _ := Integrate([]Sample{{At: jan1, Value: 8}}, from, to)
	if want := int64(8 * 3 * 3600); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestIntegrateCarriesASampleBeforeTheWindowForward(t *testing.T) {
	// A fleet that was already up on the 1st is billed from the 1st, not from
	// when Fleet happened to start watching.
	from, to := jan1, jan1.Add(time.Hour)
	got, _ := Integrate([]Sample{{At: jan1.Add(-72 * time.Hour), Value: 4}}, from, to)
	if want := int64(4 * 3600); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestIntegrateStopsAtTheNextSample(t *testing.T) {
	from, to := jan1, jan1.Add(4*time.Hour)
	samples := []Sample{atHour(0, 4), {At: jan1.Add(90 * time.Minute), Value: 2}}
	got, _ := Integrate(samples, from, to)
	// an hour and a half at 4, then two and a half hours at 2.
	want := int64(4*5400 + 2*9000)
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestIntegrateCapsAGapRatherThanBillingAllTheWayToMonthEnd(t *testing.T) {
	// The single most expensive bug this package could have: an operator who
	// switches the cluster off in week one, never reports again, and is billed
	// for the whole month because Fleet trusted its last sample.
	from, to := jan1, at("2026-01-31T00:00:00Z")
	got, _ := Integrate([]Sample{atHour(0, 8)}, from, to)
	if want := int64(8) * int64(MaxGap/time.Second); got != want {
		t.Fatalf("a stopped reporter billed %d GPU-seconds, want %d", got, want)
	}
}

func TestIntegrateSortsUnorderedSamples(t *testing.T) {
	from, to := jan1, jan1.Add(2*time.Hour)
	samples := []Sample{{At: jan1.Add(time.Hour), Value: 2}, atHour(0, 2)}
	got, _ := Integrate(samples, from, to)
	if want := int64(2*3600 + 2*3600); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestIntegrateCountsAZeroSampleAsObserved(t *testing.T) {
	// A scaled-to-zero cluster is a fact, not a gap: treating it as missing
	// would turn a mid-month shutdown into "insufficient data".
	from, to := jan1, jan1.Add(6*time.Hour)
	samples := []Sample{atHour(0, 3), {At: jan1.Add(3 * time.Hour), Value: 0}}
	got, cov := Integrate(samples, from, to)
	if want := int64(3 * 3 * 3600); got != want {
		t.Fatalf("weighted %d, want %d", got, want)
	}
	if want := 6 * time.Hour; cov.Observed != want {
		t.Fatalf("observed %v, want %v", cov.Observed, want)
	}
}

func TestIntegrateIgnoresSamplesAfterTheWindow(t *testing.T) {
	from, to := jan1, jan1.Add(time.Hour)
	samples := []Sample{atHour(0, 1), {At: jan1.Add(time.Hour), Value: 99}, {At: jan1.Add(5 * time.Hour), Value: 99}}
	got, _ := Integrate(samples, from, to)
	if want := int64(3600); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestIntegrateOnAnEmptyWindowIsZero(t *testing.T) {
	got, cov := Integrate([]Sample{atHour(0, 8)}, jan1, jan1)
	if got != 0 || cov.Total != 0 {
		t.Fatalf("got %d over %v, want zero", got, cov.Total)
	}
}
