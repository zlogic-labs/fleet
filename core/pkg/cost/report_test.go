package cost

import (
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func TestAllocateGivesTheRemainderToTheLargestFraction(t *testing.T) {
	// A pool that does not divide evenly must still add up: "the invoices do
	// not sum to the pool" is the first thing anyone checks.
	pool := billing.Amount(100)
	rows, total := allocate(pool, []Use{
		{Key: "a", GPUSeconds: 1},
		{Key: "b", GPUSeconds: 1},
		{Key: "c", GPUSeconds: 1},
	}, nil)
	var sum billing.Amount
	for _, r := range rows {
		sum += r.Amount
	}
	if sum != pool || total != pool {
		t.Fatalf("rows sum to %s (total %s), want %s", sum, total, pool)
	}
}

func TestAllocateSharesOutAMicroUnit(t *testing.T) {
	pool := billing.Amount(10)
	rows, total := allocate(pool, []Use{
		{Key: "a", GPUSeconds: 1},
		{Key: "b", GPUSeconds: 2},
	}, nil)
	var sum billing.Amount
	for _, r := range rows {
		sum += r.Amount
	}
	if sum != pool || total != pool {
		t.Fatalf("rows sum to %s (total %s), want %s", sum, total, pool)
	}
	if rows[0].Share != 333_333 || rows[1].Share != 666_666 {
		t.Fatalf("shares %d %d, want 333333 666666", rows[0].Share, rows[1].Share)
	}
}

func TestAllocateWithNothingConsumedPaysNobody(t *testing.T) {
	rows, total := allocate(billing.Amount(500), nil, nil)
	if len(rows) != 0 || total != 0 {
		t.Fatalf("got %d rows totalling %s", len(rows), total)
	}
}

func TestPercentRoundsWithoutFloats(t *testing.T) {
	cases := []struct{ part, whole, want int64 }{
		{1, 3, 33}, {2, 3, 67}, {0, 0, 0}, {1, 0, 0}, {5, 5, 100}, {-1, 2, -50},
	}
	for _, c := range cases {
		if got := int64(percent(c.part, c.whole)); got != c.want {
			t.Fatalf("percent(%d,%d) = %d, want %d", c.part, c.whole, got, c.want)
		}
	}
}

func TestParsePeriodRejectsGarbage(t *testing.T) {
	if _, err := ParsePeriod("January"); err == nil {
		t.Fatal("accepted a month name")
	}
	p, err := ParsePeriod("2026-02")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.String() != "2026-02" {
		t.Fatalf("round trip gave %q", p.String())
	}
	if got := p.End.Sub(p.Start); got != 28*24*time.Hour {
		t.Fatalf("February is %v, want 28 days", got)
	}
}

func TestPeriodSpansWholeMonthsNotRollingDays(t *testing.T) {
	p, _ := ParsePeriod("2026-01")
	if p.Contains(jan1) != true || p.Contains(at("2026-02-01T00:00:00Z")) != false {
		t.Fatal("boundaries are wrong")
	}
	if !p.Contains(p.End.Add(-time.Nanosecond)) {
		t.Fatal("the last nanosecond must be inside the period")
	}
}

// januaryHours is how long the fixture month is. Written out rather than
// derived because the tests below assert exact percentages, and "30 days" is
// what makes them wrong by a percent.
const januaryHours = 31 * 24

// steady emits samples every interval for the whole of January at a fixed
// value, which is what an operator who never changes anything looks like.
func steady(value, every int) []Sample {
	var out []Sample
	for h := 0; h <= 30*24; h += every {
		out = append(out, Sample{At: jan1.Add(time.Duration(h) * time.Hour), Value: int64(value)})
	}
	return out
}

// held covers n hours of January with one tenant on the 8-GPU deployment.
func held(key string, hours int) []Span {
	return []Span{{
		From: jan1,
		To:   jan1.Add(time.Duration(hours) * time.Hour),
		Key:  key,
		GPUs: 8,
	}}
}

// fullMonth is one cluster and one deployment, 8 GPUs sampled hourly across
// January, with a quarter of that month actually serving traffic.
func fullMonth() Input {
	p, _ := ParsePeriod("2026-01")
	return Input{
		Period:      p,
		Capacity:    map[string][]Sample{"c1": steady(8, 1)},
		Reserved:    map[string][]Sample{"llama": steady(8, 1)},
		Spans:       map[string][]Span{"llama": held("acme/research", januaryHours/4)},
		MinCoverage: DefaultMinCoverage,
	}
}

func asErr(err error, target **ErrIncomplete) bool {
	e, ok := err.(*ErrIncomplete)
	if ok {
		*target = e
	}
	return ok
}
