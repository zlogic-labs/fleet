package billing

import (
	"math"
	"sort"
)

// Two independent measurements of the same thing.
//
// When an engine reports usage, that number is what gets billed. When it does
// not, the gateway counts the answer it forwarded instead. Those two paths are
// produced by entirely different machinery — the engine's own accounting on one
// side, a tokenizer and a tap on the other — and between them sits a class of
// bug that nothing else in the system can see: a wrong tokenizer for a model,
// a delta shape the tap silently drops, a stream cut in the middle so only
// half the answer was counted.
//
// A single request cannot reveal any of that. One request measured two ways
// disagrees all the time, and disagreeing is the expected outcome for a prompt
// cache or a reasoning block whose tokens are attributed differently by each
// side. It takes a population, and it takes both populations for the same
// endpoint, before "the gateway counts answers 20% short" stops being a
// coincidence and starts being a fact.
//
// So this is a check, not a correction. Nothing here changes a price.

// Observation is what one row contributes: the tokens billed for the answer,
// and whether the engine's own account of them was available.
type Observation struct {
	Key        string
	Output     int64
	FromEngine bool
	Source     Source
	Truncated  bool
}

// Sample is one key's two populations.
type Sample struct {
	Key string
	// EngineOut and CountedOut are summed completion tokens, which is the
	// dimension that can be compared. Prompt tokens cannot: the engine counts
	// what its own prefix cache held, and the gateway has no way to know that,
	// so the two prompt figures are not measuring the same thing.
	EngineOut  int64
	EngineN    int64
	CountedOut int64
	CountedN   int64
	ReservedN  int64
	// Truncated is how many counted answers hit the tap's cap. A truncation
	// makes that key's counted total a floor, so its divergence is reported
	// but never called a fault.
	Truncated int64
}

// Divergence is one key's verdict.
type Divergence struct {
	Key string
	// Ratio is CountedOut / EngineOut. It is left unclamped: a ratio of 1.4 and
	// a ratio of 0.4 are the same size of problem in opposite directions, and
	// a report that showed only "40% off" would read as an improvement.
	Ratio float64
	// Percent is Ratio-1 as a percentage, rounded. The absolute number is what
	// an operator reads; the ratio is what a comparison across keys needs.
	Percent int
	// Fault is set when the divergence is large enough to be worth stopping
	// for. It is false for a key with too little data to mean anything.
	Fault    bool
	Reason   string
	EngineN  int64
	CountedN int64
}

// DefaultMinSamples is how many observations of each kind a key needs before
// its divergence means anything.
//
// Five is low enough that a small deployment still gets an answer on its
// busiest endpoint, and high enough that one request whose prompt cache
// behaved oddly cannot produce a finding. Below it the check reports nothing
// rather than a number with a warning attached, because a warning nobody can
// act on trains people to ignore warnings.
const DefaultMinSamples = 5

// DefaultTolerance is the divergence that counts as a fault, as a percentage.
//
// Ten percent is above the spread a tokenizer mismatch and a delta shape
// produce between two honest measurements, and below the size of a real bug: a
// gateway counting only the final delta of a stream, or resolving every model
// to a byte-per-character heuristic, lands tens of percent out rather than
// marginally.
const DefaultTolerance = 10

// Verify compares the two populations per key.
//
// It is a pure function over observations so the policy — the thresholds, the
// reasons, the ordering — can be tested without a database, and so the same
// function can be pointed at a live query or a fixture.
func Verify(obs []Observation, minSamples int, tolerancePercent int) []Divergence {
	if minSamples <= 0 {
		minSamples = DefaultMinSamples
	}

	samples := group(obs)
	keys := make([]string, 0, len(samples))
	for k := range samples {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Divergence, 0, len(keys))
	for _, k := range keys {
		s := samples[k]
		d := Divergence{Key: k, EngineN: s.EngineN, CountedN: s.CountedN}
		switch {
		case s.EngineN < int64(minSamples) || s.CountedN < int64(minSamples):
			// One side missing or thin. Not a fault: a deployment whose engine
			// always reports usage has no counted population at all, and calling
			// that a disagreement would be reporting the absence of data as a
			// defect.
			d.Reason = "not enough of both kinds on this key to compare"
		case s.EngineOut <= 0:
			d.Reason = "the engine reported no output tokens to compare against"
		case s.Truncated > 0:
			// A truncated count is a floor by construction, so it will sit
			// below the engine's figure and that is arithmetic, not a fault.
			d.Ratio = float64(s.CountedOut) / float64(s.EngineOut)
			d.Percent = percentOff(d.Ratio)
			d.Reason = "a counted answer hit the tap's cap, so its total is a floor"
		default:
			d.Ratio = float64(s.CountedOut) / float64(s.EngineOut)
			d.Percent = percentOff(d.Ratio)
			if d.Percent > tolerancePercent || d.Percent < -tolerancePercent {
				d.Fault = true
				d.Reason = "the gateway's count disagrees with the engine's account"
			} else {
				d.Reason = "within tolerance"
			}
		}
		out = append(out, d)
	}
	return out
}

func group(obs []Observation) map[string]*Sample {
	out := map[string]*Sample{}
	for _, o := range obs {
		s := out[o.Key]
		if s == nil {
			s = &Sample{Key: o.Key}
			out[o.Key] = s
		}
		switch {
		case o.FromEngine:
			s.EngineOut += o.Output
			s.EngineN++
		case o.Source == SourceCounted:
			s.CountedOut += o.Output
			s.CountedN++
			if o.Truncated {
				s.Truncated++
			}
		default:
			s.ReservedN++
		}
	}
	return out
}

// percentOff converts a ratio to a signed whole percentage.
//
// Rounded, not truncated: truncation reports every small-but-real divergence as
// exactly zero, and a check that cannot report a 1% drift is a check nobody can
// calibrate against. It also reports a real 40% as 39, because 1.4-1 is
// 0.3999999999999999 in binary floating point — which was this function's
// behaviour until a test asked for 40 and got 39.
func percentOff(ratio float64) int {
	return int(math.Round((ratio - 1) * 100))
}

// Faults filters to the keys worth stopping for.
func Faults(all []Divergence) []Divergence {
	var out []Divergence
	for _, d := range all {
		if d.Fault {
			out = append(out, d)
		}
	}
	return out
}
