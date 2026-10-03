package cost

import (
	"sort"
	"time"
)

// Measuring what a deployment's GPUs were doing, and who had them.

// Span is the interval one request occupied a deployment.
//
// GPUs is gpu_per_replica as it was when the request ran, because a deployment
// rescaled mid-period must not have last month's requests re-priced against
// today's shape.
type Span struct {
	From, To time.Time
	Key      string
	GPUs     int64
}

// Busy is the answer to "were these GPUs paid for, and were they working".
//
// Seconds is what a deployment's capacity was doing; ByKey is that same total
// split between callers and always sums to it, because it is the sum.
type Busy struct {
	Seconds int64
	ByKey   map[string]int64
}

// shareScale is the fixed-point headroom for the per-edge division, and unit
// is that scale carried through milliseconds to GPU-seconds.
//
// Every edge needs dividing by the concurrent demand, and thousands of them can
// land inside one second. Truncating each contribution to whole GPU-seconds
// would discard most of a month, so contributions are accumulated as integers
// five decimal places finer than the answer needs. A key's share of the demand
// is at most its own GPUs, which is what keeps this from overflowing: the
// edges partition the window, so the accumulator is bounded by the largest
// single share times the length of the window.
const (
	shareScale = 100_000
	unit       = shareScale * 1000
)

// Sweep integrates concurrent spans against a capacity series.
//
// Summing each request's wall clock — what this replaces — counts a GPU-second
// per request per second, so ten concurrent requests on one GPU bill ten
// GPU-hours for one. The error is the concurrency itself: it grows with load, it
// makes a saturated deployment look busier than a genuinely idle one, and it
// hands a tenant a cheaper bill for serialising their traffic. No clamp hides
// that, because the figure is wrong before it is compared with anything.
//
// The rule is, at every instant:
//
//	busy = min(capacity, demand)
//
// GPUs that are not there cannot be occupied, and GPUs nobody asked for are
// idle however many other GPUs are. A key's share of a busy instant is its own
// demand over the total, so keys sum to busy and busy never exceeds capacity —
// which is what makes the idle figure trustworthy rather than clamped.
//
// With no capacity sample covering the window the shape is unknown, and demand
// is taken at face value: inventing a smaller pool would hide the usage.
func Sweep(capacity []Sample, spans []Span, from, to time.Time) Busy {
	if !to.After(from) {
		return Busy{}
	}

	edges := make([]edge, 0, len(spans)*2+len(capacity))
	edges = append(edges, capacityEdges(capacity, from, to)...)
	edges = append(edges, spanEdges(spans, from, to)...)
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].at.Before(edges[j].at) })

	s := sweep{demand: map[string]int64{}, rate: map[string]int64{}, weighted: map[string]int64{}}
	at := from
	for _, e := range edges {
		s.hold(at, e.at)
		at = e.at
		if e.capacity {
			s.capacity, s.known = e.gpus, true
			s.rescale()
			continue
		}
		s.add(e.key, e.gpus)
	}
	s.hold(at, to)

	out := Busy{ByKey: make(map[string]int64, len(s.weighted))}
	type leftover struct {
		key string
		rem int64
	}
	fracs := make([]leftover, 0, len(s.weighted))
	var spare int64
	for k, w := range s.weighted {
		out.ByKey[k] = w / unit
		fracs = append(fracs, leftover{k, w % unit})
		spare += w % unit
	}
	// Whole GPU-seconds the fixed-point division left over, handed to the keys
	// nearest the next one. Truncating instead would make the split depend on
	// map order, and Seconds would not be the sum of its own rows. Keys break
	// ties by name so two runs of the same month give the same invoice.
	sort.Slice(fracs, func(i, j int) bool {
		if fracs[i].rem != fracs[j].rem {
			return fracs[i].rem > fracs[j].rem
		}
		return fracs[i].key < fracs[j].key
	})
	for i, left := 0, spare/unit; i < len(fracs) && left > 0; i, left = i+1, left-1 {
		out.ByKey[fracs[i].key]++
	}
	for _, v := range out.ByKey {
		out.Seconds += v
	}
	return out
}

// edge is one change on the timeline: capacity steps, or a key's demand rises or
// falls by gpus.
type edge struct {
	at   time.Time
	key  string
	gpus int64
	// capacity marks a step rather than a delta. A request can start and stop
	// at the same instant, so the two kinds cannot share one signed field.
	capacity bool
}

// capacityEdges turns the samples into the same step function Integrate reads,
// including its refusal to trust one observation for more than MaxGap.
//
// The expiry edges matter: without them a deployment that reported once at the
// start of the month and then went quiet would hold its capacity for the whole
// month here, while Integrate stops counting it after a day. The two would
// disagree, and the disagreement is a deployment with more used than reserved.
func capacityEdges(samples []Sample, from, to time.Time) []edge {
	ordered := make([]Sample, len(samples))
	copy(ordered, samples)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })

	out := make([]edge, 0, len(ordered)*2)
	for i, s := range ordered {
		at := s.At
		if at.Before(from) {
			at = from
		}
		if at.After(to) {
			break
		}
		out = append(out, edge{at: at, gpus: s.Value, capacity: true})

		var next time.Time
		if i+1 < len(ordered) {
			next = ordered[i+1].At
		}
		if next.IsZero() || next.Sub(at) > MaxGap {
			if expires := at.Add(MaxGap); !expires.After(to) {
				out = append(out, edge{at: expires, capacity: true})
			}
		}
	}
	return out
}

func spanEdges(spans []Span, from, to time.Time) []edge {
	out := make([]edge, 0, len(spans)*2)
	for _, s := range spans {
		start, end := s.From, s.To
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !end.After(start) || s.GPUs <= 0 {
			continue
		}
		out = append(out, edge{at: start, key: s.Key, gpus: s.GPUs})
		out = append(out, edge{at: end, key: s.Key, gpus: -s.GPUs})
	}
	return out
}
