package metrics

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/prom"
)

// The buckets of a cumulative histogram only ever go up.
//
// This is the one property of the exposition that no server checks and every
// dashboard assumes. Emit per-bucket counts instead and Prometheus accepts
// them, the le="+Inf" total stops being the total, and a distribution chart
// shows the request distribution shrinking as traffic grows. The round-trip
// tests in text_test.go cannot see it: a reversed histogram parses back into a
// reversed histogram.

// bucketCounts returns the cumulative bucket values of a histogram family,
// ordered by the bound they cover.
func bucketCounts(t *testing.T, exposition, family string) []float64 {
	t.Helper()
	var out []float64
	for _, s := range prom.Parse([]byte(exposition)) {
		if s.Name != family+"_bucket" {
			continue
		}
		out = append(out, s.Value)
	}
	if len(out) == 0 {
		t.Fatalf("no buckets for %s", family)
	}
	return out
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New()
	h := r.Histogram("latency_seconds", "Wall clock.",
		[]float64{0.1, 0.5, 1, 5}, "tenant")

	// One observation in each band, then a large tail: reversing the buckets
	// still produces a parseable exposition and still has the right +Inf
	// total, so only monotonicity catches it.
	for _, v := range []float64{0.05, 0.2, 0.9, 30, 30} {
		h.Observe(v, "acme")
	}

	counts := bucketCounts(t, r.String(), "latency_seconds")
	want := []float64{1, 2, 3, 3, 5}
	if len(counts) != len(want) {
		t.Fatalf("got %d buckets, want %d: %v", len(counts), len(want), counts)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Errorf("bucket %d is %v, below bucket %d at %v: %v",
				i, counts[i], i-1, counts[i-1], counts)
		}
		if counts[i] != want[i] {
			t.Errorf("bucket %d is %v, want %v: %v", i, counts[i], want[i], counts)
		}
	}
}

func TestABoundIsInclusiveSoTheLastOneIsNotAnOffByOne(t *testing.T) {
	r := New()
	h := r.Histogram("latency_seconds", "Wall clock.", []float64{0.1, 0.5}, "tenant")

	// Exactly on the boundary: le="0.1" has to include it, or every client
	// computing a quantile from the cumulative counts is off by one
	// observation.
	h.Observe(0.1, "acme")
	h.Observe(0.5, "acme")

	byBound := map[string]float64{}
	for _, s := range prom.Parse([]byte(r.String())) {
		if s.Name != "latency_seconds_bucket" {
			continue
		}
		le, ok := s.Label("le")
		if !ok {
			t.Fatal("a bucket without an le label")
		}
		byBound[le] = s.Value
	}
	if got := byBound["0.1"]; got != 1 {
		t.Errorf("le=0.1 is %v, want 1: a boundary belongs to its own bucket", got)
	}
	if got := byBound["0.5"]; got != 2 {
		t.Errorf("le=0.5 is %v, want 2", got)
	}
	if got := byBound["+Inf"]; got != 2 {
		t.Errorf("le=+Inf is %v, want 2", got)
	}
	// The literal has to be "+Inf": "inf", "+inf" and "Inf" all parse as a
	// float and none of them is what the format requires.
	if _, ok := byBound["+Inf"]; !ok {
		t.Error("the last bucket is not labelled le=\"+Inf\"")
	}
	if len(byBound) != 3 {
		t.Errorf("got %d buckets, want 3: %v", len(byBound), byBound)
	}
}
