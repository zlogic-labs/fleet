package cost

import (
	"sort"
	"time"
)

// Sample is one observation of a quantity that is assumed constant until the
// next observation.
type Sample struct {
	At    time.Time
	Value int64
}

// MaxGap caps how long a single observation is trusted.
//
// The gap the operator switched the cluster off, which reports do carry: a
// deployment scaled to zero is a sample of zero. So the cap is not the defence
// against that — the coverage gate in Close is, because a cluster that stopped
// reporting produces no samples at all and fails it. What the cap does catch is
// one absurd timestamp: a clock jump dating a sample next year would otherwise
// hold for years.
//
// A day, not an hour. An operator sampling every few hours still has to be able
// to close a month, and a cap tight enough to truncate normal reporting silently
// produces a short bill.
const MaxGap = 24 * time.Hour

// Coverage is how much of the period the samples actually account for, as a
// fraction of its length.
//
// A report computed over 40% of a month and presented as a monthly number is
// the exact failure mode this whole package exists to avoid: it looks like a
// number, it is smaller than the truth, and nobody can tell from the output.
type Coverage struct {
	Observed time.Duration
	Total    time.Duration
}

// Fraction is the observed share, 0 when the period is empty.
func (c Coverage) Fraction() float64 {
	if c.Total <= 0 {
		return 0
	}
	return float64(c.Observed) / float64(c.Total)
}

// Complete reports whether the samples account for enough of the period.
func (c Coverage) Complete(minFraction float64) bool { return c.Fraction() >= minFraction }

// Integrate sums value × time over the window, given samples of that value.
//
// It is the only place in Fleet where a time series becomes money, so it is the
// only place that has to get the edges right:
//
//   - a sample before the window starts is clamped to the window start, so a
//     fleet that was already up on the 1st is billed for the 1st;
//   - the last sample holds only to the next sample, to the window end, or to
//     MaxGap, whichever comes first;
//   - samples inside the window that report zero count as observations. A
//     scaled-to-zero cluster is a fact worth covering, and treating it as a gap
//     would let a mid-month shutdown look like missing data.
//
// Unsorted input is sorted, because the caller reads samples out of a database
// in whatever order the planner felt like.
func Integrate(samples []Sample, from, to time.Time) (int64, Coverage) {
	var cov Coverage
	if !to.After(from) {
		return 0, cov
	}
	cov.Total = to.Sub(from)

	ordered := make([]Sample, len(samples))
	copy(ordered, samples)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })

	var weighted int64

	for i, s := range ordered {
		start := s.At
		if start.Before(from) {
			start = from
		}
		if !start.Before(to) {
			break
		}
		end := to
		if i+1 < len(ordered) {
			end = ordered[i+1].At
		}
		if end.After(to) {
			end = to
		}
		if held := end.Sub(start); held > 0 {
			if held > MaxGap {
				held = MaxGap
			}
			weighted += s.Value * int64(held/time.Second)
			cov.Observed += held
		}
	}

	return weighted, cov
}
