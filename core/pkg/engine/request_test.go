package engine

import (
	"encoding/json"
	"testing"
)

func TestTheGateIsTheOnlyThingTheHotPathLooksFor(t *testing.T) {
	vllm := VLLMProfile().Request
	if got := string(vllm.GateMarker()); got != `"metrics"` {
		t.Errorf("gate marker = %s, want %q", got, `"metrics"`)
	}
	// The paths are relative to the gate, not to the frame root. A profile that
	// repeated the gate inside every path would be a second thing to keep
	// consistent, and the disagreement between them would surface as a field
	// that is silently always null.
	if got := vllm.Field(RequestQueueMS); got != "metrics.queue_time_ms" {
		t.Errorf("queue path = %q, want metrics.queue_time_ms", got)
	}
	if DefaultProfile().Request.Available() {
		t.Error("the default profile declares timings; an unknown engine should claim nothing")
	}
}

func TestAnEngineWithNoPathsIsNotSeparable(t *testing.T) {
	// llama.cpp reports a span covering queueing and prompt evaluation
	// together. Declaring it as a queue time would file a figure that no
	// measurement supports, and the console would draw a queue line for it.
	llama := LlamaCPPProfile().Request
	if llama.Paths[RequestQueueMS] != "" {
		t.Error("llama.cpp is declared to report a queue time; it does not separate one")
	}
	if llama.Paths[RequestDecodeMS] == "" {
		t.Error("llama.cpp does report a decode span and it should be declared")
	}
	if vllm := VLLMProfile().Request; !vllm.Known(RequestQueueMS, RequestDecodeMS) {
		t.Error("vLLM declares all three fields, so Known should say so")
	}
	if llama.Known(RequestQueueMS) {
		t.Error("Known claimed a field llama.cpp does not publish")
	}
}

// A null is not a zero. vLLM returns null for every field when its server was
// started without per-request metrics, which is the default, and reading that
// as 0 produces the most reassuring wrong number available.
//
// The positive assertion in the same body is not incidental: on its own the
// null check passes against a decoder that reads nothing whatsoever, which is
// what it did until a second field in the same frame was demanded.
func TestANullFieldIsAbsentRatherThanZero(t *testing.T) {
	spec := VLLMProfile().Request
	frame := decode(t, `{"metrics":{"queue_time_ms":null,"generation_time_ms":842.5}}`)
	got := spec.Timings(frame)
	if v, ok := got[RequestQueueMS]; ok {
		t.Errorf("a null queue time decoded as %v; it must be absent", v)
	}
	if got[RequestDecodeMS] != 842.5 {
		t.Errorf("decode = %v, want 842.5", got[RequestDecodeMS])
	}
}

// A zero is a legitimate measurement — a request that went straight to an idle
// engine really does have a queue time of zero, and it must survive the round
// trip. The test that only checks nulls cannot tell these two apart, so it does
// not count as covering the decode.
func TestARealZeroSurvives(t *testing.T) {
	spec := VLLMProfile().Request
	got := spec.Timings(decode(t, `{"metrics":{"queue_time_ms":0}}`))
	v, ok := got[RequestQueueMS]
	if !ok {
		t.Fatal("a measured zero queue time was dropped")
	}
	if v != 0 {
		t.Errorf("queue = %v, want 0", v)
	}
}

func TestAMissingContainerYieldsNothing(t *testing.T) {
	spec := VLLMProfile().Request
	for _, body := range []string{
		`{}`,
		`{"metrics":{}}`,
		`{"metrics":null}`,
		`{"metrics":{"queue_time_ms":"fast"}}`,
	} {
		if got := spec.Timings(decode(t, body)); len(got) != 0 {
			t.Errorf("%s decoded to %v; a frame with nothing usable should decode to nothing", body, got)
		}
	}
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		t.Fatalf("fixture %s: %v", body, err)
	}
	return frame
}
