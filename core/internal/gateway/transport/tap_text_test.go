package transport

import (
	"strings"
	"testing"
)

// The gateway counts the answer when the engine will not report it, and these
// are the shapes it has to survive: a stream split across reads, a non-streamed
// body, and the frames that carry nothing and must cost nothing.

func streamOver(t *testing.T, frames ...string) Result {
	t.Helper()
	tap := NewTap(0, nil)
	tap.UseSSE(true)
	for _, f := range frames {
		tap.observe([]byte("data: " + f + "\n\n"))
	}
	tap.seal()
	return tap.Result()
}

func TestTheTapCollectsAStreamedAnswer(t *testing.T) {
	res := streamOver(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Red"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":", Blue"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	if res.Text != "Red, Blue" {
		t.Fatalf("text = %q, want %q", res.Text, "Red, Blue")
	}
	if res.UsageKnown {
		t.Error("usage reported as known; this stream carried no usage frame")
	}
}

// A frame that carries no answer must not cost a decode. The gate is what
// keeps a thousand-token stream from being a thousand JSON parses, so it is
// worth a test that a frame with nothing in it leaves the text alone.
func TestFramesWithNothingInThemAreIgnored(t *testing.T) {
	res := streamOver(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`[DONE]`,
	)
	if res.Text != "x" {
		t.Fatalf("text = %q, want %q", res.Text, "x")
	}
}

// Reasoning content is billed as output on every provider that emits it, and a
// gateway that counted only `content` would under-report exactly the models
// that cost the most to run.
func TestReasoningContentCountsAsAnswer(t *testing.T) {
	res := streamOver(t,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think harder"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"42"}}]}`,
	)
	if res.Text != "think harder42" {
		t.Fatalf("text = %q", res.Text)
	}
}

// Tool-call arguments are output the tenant pays for. Counting only the
// assistant's prose would make an agent loop look free.
func TestToolCallArgumentsCountAsAnswer(t *testing.T) {
	res := streamOver(t,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
	)
	if !strings.Contains(res.Text, "Paris") {
		t.Fatalf("text = %q, want the arguments", res.Text)
	}
}

func TestTheTapCollectsANonStreamedAnswer(t *testing.T) {
	tap := NewTap(0, nil)
	tap.observe([]byte(`{"id":"1","object":"chat.completion","model":"demo","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"the capital of France"},"finish_reason":"stop"}]}`))
	tap.seal()
	res := tap.Result()
	if res.Text != "the capital of France" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.UsageKnown {
		t.Error("usage reported as known; this body carried none")
	}
}

// An answer longer than the tap keeps is a floor, not a measurement, and the
// result has to say so. Silently returning a truncated count as if it were the
// whole thing would make a rare case look exact.
func TestAnOverlongAnswerIsFlaggedAsTruncated(t *testing.T) {
	tap := NewTap(0, nil)
	tap.UseSSE(true)
	huge := strings.Repeat("a", textCap+10)
	tap.observe([]byte(`data: {"choices":[{"index":0,"delta":{"content":"` + huge + `"}}]}` + "\n\n"))
	tap.seal()
	res := tap.Result()
	if !res.TextTruncated {
		t.Fatal("an answer past the cap was not flagged")
	}
	if len(res.Text) != textCap {
		t.Errorf("kept %d bytes, want the cap %d", len(res.Text), textCap)
	}
}

// A response that reports usage still gets its text collected; the billing
// layer ignores it, and collecting it costs one extra decode of the final
// frame. Asserting the usage still wins is what stops a later optimisation
// from making the two compete.
func TestUsageStillWinsOverTheCountedText(t *testing.T) {
	res := streamOver(t,
		`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	)
	if !res.UsageKnown {
		t.Fatal("usage was not read")
	}
	if res.Usage.TotalTokens != 13 {
		t.Errorf("total = %d, want the engine's 13", res.Usage.TotalTokens)
	}
	if res.Text != "hello" {
		t.Errorf("text = %q; the answer should still be available", res.Text)
	}
}
