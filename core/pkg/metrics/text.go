package metrics

import (
	"io"
	"math"
	"strconv"
	"strings"
)

// The exposition writer.
//
// Format version 0.0.4, the one every Prometheus server accepts. Three rules
// that are easy to get wrong and impossible to notice until a scrape fails:
//
//   - HELP text escapes backslash and newline; label values also escape the
//     double quote.
//   - A counter's value is a float in the format, so `1` must not be written
//     as `1.0` and an integer-valued gauge must not lose precision.
//   - A histogram emits `_bucket{le="..."}` lines with *cumulative* counts,
//     then `_sum` and `_count`. Emitting per-bucket counts produces a
//     distribution that is monotonically decreasing, which is wrong in a way
//     that still renders.

// Counter is a monotonically increasing value.
type Counter struct{ f *family }

// Add increases a series. A negative delta is ignored rather than applied,
// because a counter that went down cannot be rendered by anything downstream.
func (c *Counter) Add(delta float64, labels ...string) {
	if delta < 0 {
		return
	}
	s := c.f.get(labels)
	c.f.mu.Lock()
	s.value += delta
	c.f.mu.Unlock()
}

// Inc adds one.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Gauge is a value that goes up and down.
type Gauge struct{ f *family }

// Set replaces a series' value.
func (g *Gauge) Set(v float64, labels ...string) {
	if math.IsNaN(v) {
		return
	}
	s := g.f.get(labels)
	g.f.mu.Lock()
	s.value = v
	g.f.mu.Unlock()
}

// Histogram records a distribution over fixed buckets.
type Histogram struct {
	f      *family
	bounds []float64
}

// Observe records one measurement.
func (h *Histogram) Observe(v float64, labels ...string) {
	if math.IsNaN(v) {
		return
	}
	s := h.f.get(labels)
	h.f.mu.Lock()
	// NaN and +Inf are handled by the comparison: +Inf falls through every
	// bucket into the +Inf one, which is what "at least this much" means.
	for i, b := range h.bounds {
		if v <= b {
			s.buckets[i]++
		}
	}
	s.buckets[len(h.bounds)]++
	s.sum += v
	s.count++
	h.f.mu.Unlock()
}

// ObserveSeconds is Observe for a duration, in the unit Prometheus expects.
func (h *Histogram) ObserveSeconds(v float64, labels ...string) { h.Observe(v, labels...) }

// Bounds returns the declared bucket upper bounds.
func (h *Histogram) Bounds() []float64 { return append([]float64(nil), h.bounds...) }

type familyView struct {
	name       string
	help       string
	kind       string
	labelNames []string
	bounds     []float64
	series     []*series
}

// WriteTo renders every family as a Prometheus exposition body.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	for _, f := range r.familiesSnapshot() {
		writeFamily(&b, f)
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// String renders the exposition, for tests and for logging.
func (r *Registry) String() string {
	var b strings.Builder
	for _, f := range r.familiesSnapshot() {
		writeFamily(&b, f)
	}
	return b.String()
}

func writeFamily(b *strings.Builder, f familyView) {
	b.WriteString("# HELP ")
	b.WriteString(f.name)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(f.help))
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(f.name)
	b.WriteByte(' ')
	b.WriteString(f.kind)
	b.WriteByte('\n')

	for _, s := range f.series {
		switch f.kind {
		case "histogram":
			writeHistogram(b, f, s)
		default:
			b.WriteString(f.name)
			writeLabels(b, f.labelNames, s.labels, "", "")
			b.WriteByte(' ')
			b.WriteString(format(s.value))
			b.WriteByte('\n')
		}
	}
}

func writeHistogram(b *strings.Builder, f familyView, s *series) {
	for i, bound := range f.bounds {
		b.WriteString(f.name)
		b.WriteString("_bucket")
		writeLabels(b, f.labelNames, s.labels, "le", format(bound))
		b.WriteByte(' ')
		b.WriteString(format(float64(s.buckets[i])))
		b.WriteByte('\n')
	}
	// The +Inf bucket is mandatory: a histogram without it cannot answer "how
	// many observations were there in total", and every server assumes one.
	b.WriteString(f.name)
	b.WriteString("_bucket")
	writeLabels(b, f.labelNames, s.labels, "le", "+Inf")
	b.WriteByte(' ')
	b.WriteString(format(float64(s.buckets[len(f.bounds)])))
	b.WriteByte('\n')

	b.WriteString(f.name)
	b.WriteString("_sum")
	writeLabels(b, f.labelNames, s.labels, "", "")
	b.WriteByte(' ')
	b.WriteString(format(s.sum))
	b.WriteByte('\n')

	b.WriteString(f.name)
	b.WriteString("_count")
	writeLabels(b, f.labelNames, s.labels, "", "")
	b.WriteByte(' ')
	b.WriteString(format(float64(s.count)))
	b.WriteByte('\n')
}

// writeLabels renders the label set, optionally plus one extra pair.
//
// The extra pair is `le`, appended last rather than sorted into place,
// because its value is a number and a sorted label set would put `le` wherever
// its name happens to sort — which differs between the per-bucket lines and
// the `_sum` line, and a metric whose label order varies between lines is
// rejected by the server.
func writeLabels(b *strings.Builder, names, values []string, extraName, extraValue string) {
	if len(names) == 0 && extraName == "" {
		return
	}
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(extraValue)
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// format renders a float the way the exposition expects: shortest round-trip,
// so 1 does not become 1.0 and 0.1 does not become 0.10000000000000001.
func format(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
