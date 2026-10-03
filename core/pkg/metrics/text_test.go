package metrics_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/metrics"
	"github.com/zlogic-labs/fleet/core/pkg/prom"
)

// The exposition is verified by parsing it back with pkg/prom -- the parser
// Fleet already trusts on an engine's /metrics.
//
// Writing a hand-rolled exporter and then eyeballing its output is how a
// format error ships: an unescaped newline, a non-cumulative bucket, a missing
// +Inf. None of those fail here. They fail at a scrape, minutes later, with
// nothing pointing back at the line that caused them.

func buckets() []float64 {
	return []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
}

func TestTheExpositionParsesBackAsWhatWasRecorded(t *testing.T) {
	r := metrics.New()
	reqs := r.Counter("fleet_requests_total", "Requests served.", "tenant", "model", "outcome")
	tokens := r.Counter("fleet_tokens_total", "Tokens billed.", "tenant", "model", "kind")
	dur := r.Histogram("fleet_request_duration_seconds", "Wall clock.", buckets(), "tenant", "model")

	reqs.Inc("acme", "gpt-4o", "ok")
	reqs.Inc("acme", "gpt-4o", "ok")
	reqs.Inc("acme", "gpt-4o", "refused")
	tokens.Add(120, "acme", "gpt-4o", "prompt")
	tokens.Add(30, "acme", "gpt-4o", "completion")
	dur.Observe(0.08, "acme", "gpt-4o")
	dur.Observe(4.0, "acme", "gpt-4o")

	samples := prom.Parse([]byte(r.String()))
	if len(samples) == 0 {
		t.Fatal("nothing parsed back")
	}

	// The counters survive the round trip with their labels intact.
	ok, found := find(samples, "fleet_requests_total", map[string]string{
		"tenant": "acme", "model": "gpt-4o", "outcome": "ok"})
	if !found || ok != 2 {
		t.Errorf("requests for a successful call = %v (found %v), want 2", ok, found)
	}
	// One per token kind rather than a summed total, because fresh prompt
	// tokens and cached ones are billed differently and a sum destroys that.
	cached, found := find(samples, "fleet_tokens_total", map[string]string{
		"tenant": "acme", "model": "gpt-4o", "kind": "prompt"})
	if !found || cached != 120 {
		t.Errorf("prompt tokens = %v (found %v), want 120", cached, found)
	}

	count := histogramSum(samples, "fleet_request_duration_seconds", "_count")
	if count != 2 {
		t.Errorf("histogram counted %v observations, want 2", count)
	}
	// Cumulative, not per-bucket: the value at a bucket is "at most this".
	// A server accepts either and renders the distribution wrong.
	le025, _ := find(samples, "fleet_request_duration_seconds_bucket", map[string]string{
		"tenant": "acme", "model": "gpt-4o", "le": "0.25"})
	if le025 != 1 {
		t.Errorf("observations at or under 0.25s = %v, want 1", le025)
	}
	leInf, found := find(samples, "fleet_request_duration_seconds_bucket", map[string]string{
		"tenant": "acme", "model": "gpt-4o", "le": "+Inf"})
	if !found || leInf != 2 {
		t.Errorf("+Inf bucket = %v (found %v), want 2 and present", leInf, found)
	}
	if sum := histogramSum(samples, "fleet_request_duration_seconds", "_sum"); math.Abs(sum-4.08) > 1e-9 {
		t.Errorf("histogram sum = %v, want 4.08", sum)
	}
}

func find(samples []prom.Sample, name string, want map[string]string) (float64, bool) {
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		all := true
		for k, v := range want {
			if got, ok := s.Label(k); !ok || got != v {
				all = false
				break
			}
		}
		if all {
			return s.Value, true
		}
	}
	return 0, false
}

func histogramSum(samples []prom.Sample, base, suffix string) float64 {
	total := 0.0
	for _, s := range samples {
		if s.Name == base+suffix {
			total += s.Value
		}
	}
	return total
}

// A label value is attacker-controlled in the sense that a tenant id is chosen
// by whoever creates the tenant. An unescaped newline splits one series into two
// malformed lines, and the scrape stops at the first one.
func TestALabelValueCannotBreakTheLineItIsOn(t *testing.T) {
	r := metrics.New()
	c := r.Counter("fleet_requests_total", "Requests.", "tenant")
	c.Inc("acme\n# TYPE fleet_fake 1\nfleet_fake")

	body := r.String()
	if strings.Count(body, "\n") != 3 {
		t.Errorf("a newline in a label value produced %d lines, want 3:\n%s",
			strings.Count(body, "\n"), body)
	}
	// The injected text legitimately appears inside the label value; what must
	// not happen is it appearing as the start of a metric line, which is what
	// an unescaped newline would produce.
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.HasPrefix(line, "fleet_fake") {
			t.Errorf("a label value injected a second metric:\n%s", body)
		}
	}
	for _, s := range prom.Parse([]byte(body)) {
		if s.Name == "fleet_requests_total" {
			if got, _ := s.Label("tenant"); got != "acme\n# TYPE fleet_fake 1\nfleet_fake" {
				t.Errorf("the label did not round-trip: %q", got)
			}
			return
		}
	}
	t.Error("the series with the awkward label value vanished")
}

func TestAQuoteInALabelValueRoundTrips(t *testing.T) {
	r := metrics.New()
	c := r.Counter("fleet_requests_total", "Requests.", "tenant")
	// Ending in a quote as well, because that is the case the parser got wrong
	// before: a value ending in an escaped quote ends in `\""`, and trimming
	// the surrounding quotes as a run leaves a dangling backslash.
	c.Inc(`say "hi"`)

	for _, s := range prom.Parse([]byte(r.String())) {
		if got, ok := s.Label("tenant"); ok {
			if got != `say "hi"` {
				t.Errorf("label round-tripped as %q, want %q", got, `say "hi"`)
			}
			return
		}
	}
	t.Errorf("the series vanished:\n%s", r.String())
}

// The wrong number of label values silently produces a series whose label set
// differs from its family's, and every Prometheus server rejects it at scrape
// time with no reference to the code. Panicking at the call site is cheaper.
func TestTheWrongNumberOfLabelsFailsAtTheCallSite(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a counter accepted the wrong number of label values")
		}
	}()
	r := metrics.New()
	c := r.Counter("fleet_requests_total", "Requests.", "tenant", "model")
	c.Inc("only-one")
}

func TestTwoScrapesOfTheSameStateAreByteIdentical(t *testing.T) {
	// Map iteration order would otherwise make every diff between two scrapes
	// a diff, which is how a real change gets missed in a noisy output.
	build := func() *metrics.Registry {
		r := metrics.New()
		g := r.Gauge("fleet_endpoint_queue_depth", "Queue.", "endpoint", "model")
		for _, ep := range []string{"d", "c", "b", "a", "e"} {
			g.Set(1, ep, "m")
		}
		return r
	}
	if build().String() != build().String() {
		t.Error("two identical registries rendered differently")
	}
}

func TestACounterRefusesToGoBackwards(t *testing.T) {
	r := metrics.New()
	c := r.Counter("fleet_requests_total", "Requests.")
	c.Add(5)
	c.Add(-3)
	if got, _ := find(prom.Parse([]byte(r.String())), "fleet_requests_total", nil); got != 5 {
		t.Errorf("counter = %v after a negative delta, want 5", got)
	}
}

func TestAGaugeFollowsItselfBackDown(t *testing.T) {
	r := metrics.New()
	g := r.Gauge("fleet_endpoint_queue_depth", "Queue.")
	g.Set(7)
	g.Set(0)
	if got, _ := find(prom.Parse([]byte(r.String())), "fleet_endpoint_queue_depth", nil); got != 0 {
		t.Errorf("gauge = %v after being zeroed, want 0", got)
	}
}

func TestSecondsRenderAsSeconds(t *testing.T) {
	// The unit suffix is the contract: a duration observed in nanoseconds and
	// written into a metric named _seconds is off by a factor of a billion,
	// and every dashboard built on it is wrong in a way nothing reports.
	r := metrics.New()
	h := r.Histogram("fleet_request_duration_seconds", "Wall clock.", buckets())
	h.ObserveSeconds(time.Second.Seconds())
	h.ObserveSeconds(2 * time.Second.Seconds())
	if got := histogramSum(prom.Parse([]byte(r.String())), "fleet_request_duration_seconds", "_sum"); got != 3 {
		t.Errorf("sum = %v seconds, want 3", got)
	}
}

func TestUnsortedBucketsAreSortedRatherThanRejected(t *testing.T) {
	r := metrics.New()
	h := r.Histogram("fleet_request_duration_seconds", "Wall clock.", []float64{1, 0.1, 10})
	b := h.Bounds()
	if b[0] != 0.1 || b[1] != 1 || b[2] != 10 {
		t.Errorf("bounds = %v, want ascending", b)
	}
}
