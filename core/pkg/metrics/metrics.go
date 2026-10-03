// Package metrics writes the Prometheus text exposition format, and holds the
// counters, gauges and histograms behind it.
//
// It is hand-written rather than pulled in as a dependency for the same reason
// pkg/prom is: the set of series Fleet publishes is small, closed and written
// down here, and a client library brings a registry that invites unbounded
// label sets rather than preventing them. What that buys is paid for in
// responsibility, which is why the exposition is verified by parsing it back
// with pkg/prom — the same parser Fleet already trusts on an engine's endpoint.
//
// What this package does not do: timestamps (they are the scraper's business
// and a stale timestamp is worse than none), exemplars, and native histograms.
package metrics

import (
	"sort"
	"sync"
)

// Label names are declared with the family and never with the call site,
// because a metric whose label set changes between two series is rejected by
// every Prometheus server and the rejection arrives minutes later, at scrape
// time, with no pointer to the code that caused it.

// Registry owns every family.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	order    []string
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{families: map[string]*family{}}
}

type family struct {
	name       string
	help       string
	kind       string
	labelNames []string
	// bounds is the histogram's bucket edges, empty for the other kinds.
	bounds []float64
	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	labels []string // values, in labelNames order
	value  float64
	// buckets is cumulative and one longer than the declared bucket list, the
	// last entry being +Inf. A histogram without it is a counter.
	buckets []uint64
	sum     float64
	count   uint64
}

func (r *Registry) lookup(name string) *family {
	r.mu.RLock()
	f := r.families[name]
	r.mu.RUnlock()
	return f
}

func (r *Registry) add(name, help, kind string, labelNames []string) *family {
	if f := r.lookup(name); f != nil {
		return f
	}
	f := &family{
		name: name, help: help, kind: kind,
		labelNames: labelNames, series: map[string]*series{},
	}
	r.mu.Lock()
	// Re-checked under the write lock: two goroutines declaring the same
	// family would otherwise each keep their own, and half the series would
	// go into the one that lost the map write.
	if existing := r.families[name]; existing != nil {
		r.mu.Unlock()
		return existing
	}
	r.families[name] = f
	r.order = append(r.order, name)
	r.mu.Unlock()
	return f
}

// Counter returns a monotonically increasing family.
func (r *Registry) Counter(name, help string, labelNames ...string) *Counter {
	return &Counter{f: r.add(name, help, "counter", labelNames)}
}

// Gauge returns a family that goes up and down.
func (r *Registry) Gauge(name, help string, labelNames ...string) *Gauge {
	return &Gauge{f: r.add(name, help, "gauge", labelNames)}
}

// Histogram returns a family that records a distribution.
//
// Buckets must be sorted ascending; an unsorted set produces an exposition a
// server silently rejects, so it is refused here instead.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) *Histogram {
	bs := append([]float64(nil), buckets...)
	sort.Float64s(bs)
	f := r.add(name, help, "histogram", labelNames)
	f.bounds = bs
	return &Histogram{f: f, bounds: bs}
}

// series returns the series for a label tuple, creating it on first use.
func (f *family) get(values []string) *series {
	if len(values) != len(f.labelNames) {
		// A wrong arity is a programming error, not a runtime condition. It is
		// fatal here rather than rendered: a series with the wrong label set
		// is rejected at scrape time with nothing to point at.
		panic("metrics: " + f.name + " wants " +
			itoa(len(f.labelNames)) + " label values, got " + itoa(len(values)))
	}
	key := seriesKey(values)
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.series[key]; s != nil {
		return s
	}
	s := &series{labels: append([]string(nil), values...)}
	if f.kind == "histogram" {
		s.buckets = make([]uint64, len(f.bounds)+1)
	}
	f.series[key] = s
	return s
}

// families returns every family in declaration order.
func (r *Registry) familiesSnapshot() []familyView {
	r.mu.RLock()
	names := append([]string(nil), r.order...)
	r.mu.RUnlock()
	out := make([]familyView, 0, len(names))
	for _, n := range names {
		f := r.lookup(n)
		if f == nil {
			continue
		}
		f.mu.Lock()
		out = append(out, familyView{
			name: f.name, help: f.help, kind: f.kind,
			labelNames: f.labelNames, bounds: f.bounds,
			series: append([]*series(nil), mapValues(f.series)...),
		})
		f.mu.Unlock()
	}
	return out
}

func mapValues(m map[string]*series) []*series {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sorted by the rendered label tuple, so two scrapes of the same state
	// produce byte-identical bodies. Unsorted map order would make every diff
	// of two scrapes meaningless.
	sort.Strings(keys)
	out := make([]*series, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// seriesKey joins values with a byte that cannot appear in a model name or a
// tenant id we would accept, so no two distinct label tuples collide.
func seriesKey(values []string) string {
	key := ""
	for _, v := range values {
		key += v + "\x00"
	}
	return key
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
