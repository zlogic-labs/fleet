package engine

import (
	"encoding/json"
	"strings"
)

// RequestField is a per-request timing Fleet wants, named independently of
// whatever the engine calls it.
type RequestField string

const (
	// RequestQueueMS is time the engine had the request and was not decoding
	// it. It is the only one of these the gateway cannot measure: it sees the
	// moment it forwarded the request and the moment the first token arrived,
	// and everything between is queue plus prefill with no boundary it can
	// observe. An engine that reports it is the only thing that can answer
	// "why was this slow" when time to first token is high.
	RequestQueueMS RequestField = "queue_ms"
	// RequestTTFTMS is the engine's own time to first token. The gateway
	// measures this too, and the two differ by network and by where each one
	// starts its clock. Recording both is what tells an operator which of the
	// two to trust.
	RequestTTFTMS RequestField = "ttft_ms"
	// RequestDecodeMS is the engine's decode interval, excluding queue and
	// prefill. Also derivable from the gateway's own numbers, and kept for the
	// same reason: when the two disagree the gateway's split of a request into
	// "waiting" and "producing" is wrong, which is exactly the split the
	// console's cost model rests on.
	RequestDecodeMS RequestField = "decode_ms"
)

// RequestSpec says where an engine puts its per-request timings.
//
// Gate is the name of the top-level object, and Paths are dotted paths relative
// to it. vLLM calls the object "metrics" and puts queue_time_ms inside;
// llama-server calls it "timings" and puts prompt_ms there. Declaring the
// container separately is what keeps the hot path cheap: the gate is one
// substring test per frame, in the same spirit as the usage gate, and a stream
// that emits hundreds of deltas never reaches the JSON parser for timing work
// unless the container name appears in the frame at all.
//
// Every field is optional and absence is meaningful. vLLM leaves them null
// unless the server was started with enable_per_request_metrics, which defaults
// to off, and it suppresses them entirely for n > 1. llama-server reports
// prompt_ms as queue plus prompt evaluation, which is not the same quantity and
// must not be filed as it. A field that is null is "the engine did not measure
// this", never zero.
type RequestSpec struct {
	Gate  string
	Paths map[RequestField]string
	// Note is shown in the console next to the engine's timings.
	Note string
}

// Available reports whether an engine publishes per-request timings at all.
func (r RequestSpec) Available() bool { return r.Gate != "" && len(r.Paths) > 0 }

// Field resolves one field's path to a locator under the gate, or "" when this
// engine does not publish it.
//
// The dot is the separator and the gate is not repeated: declaring "metrics" /
// "metrics.queue_time_ms" would make the gate a second thing to keep consistent
// with the paths, and a disagreement between them would surface as a field that
// is always null.
func (r RequestSpec) Field(f RequestField) string {
	if r.Gate == "" {
		return ""
	}
	path := r.Paths[f]
	if path == "" {
		return ""
	}
	return r.Gate + "." + path
}

// GateMarker is the byte sequence to look for in a frame before decoding it.
// Empty when nothing is published, so the tap can skip the whole step.
func (r RequestSpec) GateMarker() []byte {
	if !r.Available() {
		return nil
	}
	return []byte(`"` + r.Gate + `"`)
}

// Known reports whether an engine publishes every field Fleet asks for.
//
// It is used by the console rather than by billing: an engine that reports two
// of the three is worse than useless if presented as one that reports all
// three, because the missing one is then read as a fast queue.
func (r RequestSpec) Known(names ...RequestField) bool {
	if !r.Available() {
		return false
	}
	for _, n := range names {
		if r.Paths[n] == "" {
			return false
		}
	}
	return true
}

// Timings decodes the declared paths out of a decoded frame.
//
// The declared paths are relative to the gate, so this descends through the gate
// first. Starting the walk at the frame root is a mistake that is invisible in
// review and lethal in practice: every field comes back absent, an absent field
// is the correct reading for a null, and so a test asserting that a null is not
// read as zero passes against a decoder that reads nothing at all.
func (r RequestSpec) Timings(frame map[string]any) map[RequestField]float64 {
	if !r.Available() {
		return nil
	}
	body, ok := frame[r.Gate].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[RequestField]float64, len(r.Paths))
	for field, path := range r.Paths {
		if v, ok := lookup(body, strings.Split(path, ".")); ok {
			out[field] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookup walks a decoded object, treating JSON null as absent.
//
// The null case is the whole reason this is separate from a plain index: vLLM
// returns null for every field when per-request metrics are off, and a float
// read of null is zero. Zero queue time on a request that waited four seconds
// is a lie that reads as good news.
func lookup(obj map[string]any, path []string) (float64, bool) {
	var cur any = obj
	for _, part := range path {
		step, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = step[part]
		if !ok {
			return 0, false
		}
	}
	switch v := cur.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
