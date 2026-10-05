package transport

import (
	"bytes"
	"encoding/json"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// EngineTimings is what the engine itself said about one request.
//
// Every field is a pointer because null is the answer engines actually give.
// vLLM returns null for all of them unless the server was started with
// enable_per_request_metrics, which is off by default, and llama-server omits
// any field whose divisor it does not know. A plain float64 would make both
// read as zero, and zero queue time is the most reassuring wrong answer
// available: a request that waited four seconds behind a full batch would be
// filed as one that went straight to work.
//
// The gateway measures time to first token and decode on every request
// regardless. These are the engine's own figures for the same spans, kept
// because the two should agree and when they do not, the disagreement says
// which one is lying. The one figure the gateway has no way to get is queue
// time, which is why it is first.
type EngineTimings struct {
	QueueMS  *float64
	TTFTMS   *float64
	DecodeMS *float64
}

// considerTimings looks for the engine's per-request timings in one frame.
//
// Gated on the container name, the same way usage is gated on its own marker:
// a stream emits hundreds of frames and at most one of them carries timings, so
// the JSON parser stays off the hot path unless the name appears at all.
//
// Last frame wins. An engine that reports on every frame is telling us the
// same thing repeatedly, and an engine that reports once on the last frame is
// the case RequestSpec exists to describe; overwriting handles both without
// needing to know which one this is.
func (t *Tap) considerTimings(payload []byte, spec engine.RequestSpec) {
	if !t.timingsSeen && spec.GateMarker() == nil {
		return
	}
	if !bytes.Contains(payload, spec.GateMarker()) {
		return
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}
	got := spec.Timings(frame)
	if len(got) == 0 {
		return
	}
	t.timings, t.timingsSeen = engineTimings(got), true
}

// engineTimings converts the decoded map into the result shape.
//
// A field the engine did not send stays nil rather than becoming zero. That is
// the whole point of the pointer: a decode time of zero and a decode time that
// was never measured are different facts, and only one of them is a problem.
func engineTimings(got map[engine.RequestField]float64) *EngineTimings {
	out := &EngineTimings{}
	for field, v := range got {
		value := v
		switch field {
		case engine.RequestQueueMS:
			out.QueueMS = &value
		case engine.RequestTTFTMS:
			out.TTFTMS = &value
		case engine.RequestDecodeMS:
			out.DecodeMS = &value
		}
	}
	return out
}

// setTimings tells the tap which engine's spec to read, before the first
// frame arrives. The proxy calls it once per request; a request whose endpoint
// declares nothing reads no timings at all.
func (t *Tap) setTimings(spec engine.RequestSpec) {
	t.request = spec
	t.timingsMarker = spec.GateMarker()
}
