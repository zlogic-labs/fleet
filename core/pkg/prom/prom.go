// Package prom parses just enough of the Prometheus text exposition format to
// read a handful of gauges off an engine's /metrics.
//
// It exists because engine metrics are a dependency we do not control the
// shape of, and pulling in a full Prometheus client library to read four
// numbers would put a large dependency tree under P1, where the whole point is
// that the protocol surface stays small. The format is a line-based subset and
// the subset is genuinely small.
//
// What is deliberately not here: timestamps, exemplars, and float
// special-value spellings ("+Inf", "NaN"). An engine that reports a queue depth
// of NaN is broken in a way Fleet cannot usefully report anyway, and returning
// no value for it is more honest than returning a number that is not.
package prom

import (
	"strconv"
	"strings"
)

// Sample is one exposed time series.
type Sample struct {
	// Name is the metric name without labels, e.g. "vllm:kv_cache_usage_perc".
	Name string
	// Labels are the exposition's label pairs, unescaped.
	Labels map[string]string
	Value  float64
}

// Label returns a label value, and whether it was present. Presence matters:
// vLLM's cache_config_info renders unset fields as the literal string "None",
// so "the label is absent" and "the label says None" are different states and
// the caller has to be able to tell them apart.
func (s Sample) Label(name string) (string, bool) {
	v, ok := s.Labels[name]
	return v, ok
}

// Parse reads an exposition body into its samples.
//
// Labels on one series may repeat across lines when the engine exposes a time
// series; Prometheus clients label those with a suffix we do not interpret. The
// first occurrence of a name wins, so a caller asking a question about a gauge
// gets a stable answer rather than whichever scrape landed last.
func Parse(body []byte) []Sample {
	var out []Sample
	for line := range lines(body) {
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		s, ok := parseLine(string(line))
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// Find returns the first sample with this name, and whether one was present.
func Find(samples []Sample, name string) (Sample, bool) {
	for _, s := range samples {
		if s.Name == name {
			return s, true
		}
	}
	return Sample{}, false
}

// Value returns a gauge's value by metric name, or false when it is absent.
//
// Absent is not zero. An engine that publishes no metrics at all must be able
// to say so, or an autoscaler will read the zero as a real measurement and act
// on it — which is how a deployment ends up with a hundred replicas of an
// engine that never published anything.
func Value(samples []Sample, name string) (float64, bool) {
	s, ok := Find(samples, name)
	if !ok {
		return 0, false
	}
	return s.Value, true
}

// IntLabel reads a label that should hold a number, and reports whether it
// did. The engine stringifies every CacheConfig field, so "8" must become 8
// while "None" must stay absent rather than becoming a fabricated zero.
func IntLabel(s Sample, name string) (int64, bool) {
	raw, ok := s.Label(name)
	if !ok || raw == "" || raw == "None" {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// FloatLabel is IntLabel for a value that can be fractional.
func FloatLabel(s Sample, name string) (float64, bool) {
	raw, ok := s.Label(name)
	if !ok || raw == "" || raw == "None" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ── internals ──────────────────────────────────────────────────

// lines iterates body without allocating a slice of every line, which matters
// because an engine's /metrics is tens of thousands of lines and the gateway
// scrapes it every few seconds.
func lines(body []byte) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(body); i++ {
			if body[i] != '\n' {
				continue
			}
			if !yield(strings.TrimSpace(string(body[start:i]))) {
				return
			}
			start = i + 1
		}
		if start < len(body) {
			yield(strings.TrimSpace(string(body[start:])))
		}
	}
}

// parseLine reads `name{labels} value`, tolerating an absent label set and a
// trailing timestamp.
func parseLine(line string) (Sample, bool) {
	open := strings.IndexByte(line, '{')
	close := strings.LastIndexByte(line, '}')

	var name, labelPart, valuePart string
	switch {
	case open >= 0 && close > open:
		name = line[:open]
		labelPart = line[open+1 : close]
		valuePart = line[close+1:]
	default:
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return Sample{}, false
		}
		name, valuePart = fields[0], strings.Join(fields[1:], " ")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Sample{}, false
	}

	fields := strings.Fields(valuePart)
	if len(fields) == 0 {
		return Sample{}, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Sample{}, false
	}
	return Sample{Name: name, Labels: parseLabels(labelPart), Value: v}, true
}

// parseLabels reads `k="v",k2="v2"` with the escapes Prometheus defines.
func parseLabels(part string) map[string]string {
	if strings.TrimSpace(part) == "" {
		return nil
	}
	out := make(map[string]string)
	for _, kv := range splitLabels(part) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[unescape(strings.TrimSpace(k))] = unescape(strings.Trim(strings.TrimSpace(v), `"`))
	}
	return out
}

// splitLabels splits on commas that are not inside a quoted value, because a
// label value may itself contain one.
func splitLabels(part string) []string {
	var out []string
	inQuote, escaped, start := false, false, 0
	for i := 0; i < len(part); i++ {
		switch c := part[i]; {
		case escaped:
			escaped = false
		case c == '\\' && inQuote:
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, part[start:i])
			start = i + 1
		}
	}
	return append(out, part[start:])
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
