package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// streamTimings feeds a whole SSE body a byte at a time, so no frame arrives
// whole and the reassembly across reads is exercised rather than assumed. It
// also matters for timings specifically: vLLM and llama-server both send theirs
// on the final frame, so a tap that looked only at the first would find nothing.
func streamTimings(t *testing.T, tap *Tap, body string) {
	t.Helper()
	body0 := &oneByteCloser{r: strings.NewReader(body)}
	if _, err := io.Copy(io.Discard, tap.Reader(body0)); err != nil {
		t.Fatalf("stream: %v", err)
	}
}

// oneByteCloser hands over one byte at a time so a frame is never whole, and
// satisfies Close so the tap can wrap it like a real body. iotest.OneByteReader
// returns a plain io.Reader and cannot be passed to Tap.Reader, which is the
// point of the seam: the tap has no way to read a body it was not given.
type oneByteCloser struct{ r io.Reader }

func (o *oneByteCloser) Read(p []byte) (int, error) { return o.r.Read(p[:1]) }
func (o *oneByteCloser) Close() error               { return nil }

func TestTheTimingsAreReadOffTheLastFrame(t *testing.T) {
	tap := NewTap(0, nil)
	tap.RequestTimings(engine.VLLMProfile().Request)
	tap.UseSSE(true)
	streamTimings(t, tap, `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{"content":" there"}}],"metrics":{"queue_time_ms":4102.5,"time_to_first_token_ms":4300,"generation_time_ms":842}}

data: [DONE]

`)
	got := tap.Result()
	if got.Engine == nil {
		t.Fatal("no engine timings read from a frame that carried them")
	}
	if got.Engine.QueueMS == nil || *got.Engine.QueueMS != 4102.5 {
		t.Errorf("queue = %v, want 4102.5", got.Engine.QueueMS)
	}
	if got.Engine.DecodeMS == nil || *got.Engine.DecodeMS != 842 {
		t.Errorf("decode = %v, want 842", got.Engine.DecodeMS)
	}
}

// Which frame wins only matters when more than one carries the container, so
// this feeds two that disagree. vLLM and llama-server each send theirs once, at
// the end -- which is exactly why a tap that latched the first frame it saw
// would look correct against both of them and be wrong against the next engine
// that reports per frame, where every earlier value is an intermediate one.
func TestWhenSeveralFramesCarryTimingsTheLastOneWins(t *testing.T) {
	tap := NewTap(0, nil)
	tap.RequestTimings(engine.VLLMProfile().Request)
	tap.UseSSE(true)
	streamTimings(t, tap, `data: {"choices":[{"delta":{"content":"a"}}],"metrics":{"queue_time_ms":5,"generation_time_ms":100}}

data: {"choices":[{"delta":{"content":"b"}}],"metrics":{"queue_time_ms":5,"generation_time_ms":900}}

data: [DONE]

`)
	got := tap.Result().Engine
	if got == nil {
		t.Fatal("no timings read")
	}
	if got.DecodeMS == nil || *got.DecodeMS != 900 {
		t.Errorf("decode = %v, want the last frame's 900 rather than the first frame's 100",
			got.DecodeMS)
	}
}

// The hot path claim: a stream of hundreds of deltas must not reach the JSON
// parser for timings. The gate is what makes that true, and this is the only
// place it can be observed — the correctness tests pass either way.
func TestADeltaWithoutTheGateIsNotDecoded(t *testing.T) {
	tap := NewTap(0, nil)
	tap.RequestTimings(engine.VLLMProfile().Request)
	tap.UseSSE(true)
	streamTimings(t, tap, `data: {"choices":[{"delta":{"content":"metrics of opinion"}}]}`+"\n\n")
	if tap.Result().Engine != nil {
		t.Error("a delta that merely contains the word metrics was read as timings")
	}
}

func TestAnEngineThatPublishesNothingLeavesTheResultNil(t *testing.T) {
	tap := NewTap(0, nil)
	// What the default profile resolves to for an endpoint carrying no engine
	// label, and what an engine Fleet has never heard of gets.
	tap.RequestTimings(engine.RequestSpec{})
	tap.UseSSE(true)
	streamTimings(t, tap,
		`data: {"choices":[{"delta":{"content":"x"}}],"timings":{"prompt_ms":10,"predicted_ms":5}}`+"\n\n")
	if got := tap.Result(); got.Engine != nil {
		t.Errorf("read %+v from a frame for an engine with no declared paths", got.Engine)
	}
}

// The gateway cannot ask every engine to behave, so it has to survive one that
// sends the shape anyway: llama-server always emits timings, and the tap has to
// leave the undeclared ones alone rather than file prompt_ms as a queue time.
func TestUndeclaredTimingsAreNotGuessedAt(t *testing.T) {
	tap := NewTap(0, nil)
	tap.RequestTimings(engine.LlamaCPPProfile().Request)
	tap.UseSSE(true)
	streamTimings(t, tap,
		`data: {"choices":[{"delta":{"content":"x"}}],"timings":{"prompt_ms":10,"predicted_ms":5}}`+"\n\n")
	got := tap.Result().Engine
	if got == nil {
		t.Fatal("the declared decode span was not read")
	}
	if got.QueueMS != nil {
		t.Errorf("queue = %v; llama.cpp's prompt_ms spans queueing and prefill and is not a queue time", *got.QueueMS)
	}
	if got.DecodeMS == nil || *got.DecodeMS != 5 {
		t.Errorf("decode = %v, want 5", got.DecodeMS)
	}
}

func TestBlockingResponsesCarryTimingsToo(t *testing.T) {
	tap := NewTap(0, nil)
	tap.RequestTimings(engine.VLLMProfile().Request)
	if _, err := io.Copy(io.Discard, tap.Reader(io.NopCloser(strings.NewReader(
		`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":3,`+
			`"completion_tokens":1,"total_tokens":4},"metrics":{"queue_time_ms":7}}`)))); err != nil {
		t.Fatalf("body: %v", err)
	}
	got := tap.Result()
	if got.Engine == nil || got.Engine.QueueMS == nil || *got.Engine.QueueMS != 7 {
		t.Errorf("blocking response lost its queue time: %+v", got.Engine)
	}
}

// The proxy is what resolves an endpoint's engine to a spec, so the wiring is
// worth covering: an endpoint carrying no engine label must still resolve,
// through the default profile, to a spec that claims nothing rather than fail.
func TestTheProxyResolvesTheEngineFromTheEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"}}],"metrics":{"queue_time_ms":12}}`+"\n\n")
	}))
	defer upstream.Close()

	ep := engine.Endpoint{
		ID: "e", Model: "m", BaseURL: upstream.URL,
		Labels: map[string]string{"engine": "vllm"},
	}
	tap := NewTap(0, nil)
	p := New(Options{Profiles: engine.BuiltinProfiles()})
	p.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ep, tap)

	if got := tap.Result().Engine; got == nil || got.QueueMS == nil || *got.QueueMS != 12 {
		t.Errorf("the proxy did not carry the endpoint's timings spec through: %+v", got)
	}
}
