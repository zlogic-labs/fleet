package transport

import (
	"io"
	"strings"
	"testing"
)

// An embeddings reply is decoded as a chat completion, and it reads correctly.
//
// There is one decode path for both response shapes because `usage` sits at the
// top level of both with the same shape, and encoding/json ignores the fields
// that differ. A shape-specific path was written here first, on the belief that
// a chat decode would find no usage and silently fall back to billing the
// reservation — which for an embedding is close enough to be invisible. The
// test that assumed the opposite is the one that found it.

const embeddingsBody = `{"object":"list","data":[{"object":"embedding","index":0,` +
	`"embedding":[0.1,0.2]}],"model":"m",` +
	`"usage":{"prompt_tokens":37,"completion_tokens":0,"total_tokens":37}}`

func tapOver(t *testing.T, tap *Tap, body string) Result {
	t.Helper()
	rc := tap.Reader(io.NopCloser(strings.NewReader(body)))
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	return tap.Result()
}

func TestTheTapReadsAnEmbeddingsUsage(t *testing.T) {
	// An embedding's whole cost is its input, so losing this number does not
	// mean an estimate is used — it means the request is billed at the
	// reservation and recorded as usage_known=false.
	res := tapOver(t, NewTap(0, nil), embeddingsBody)
	if !res.UsageKnown {
		t.Fatal("usage reported as unknown; the request would be billed from its reservation")
	}
	if res.Usage == nil || res.Usage.PromptTokens != 37 {
		t.Fatalf("usage = %+v, want 37 prompt tokens", res.Usage)
	}
}

func TestAnEmbeddingsReplyWithNoUsageIsNotChargedAsKnown(t *testing.T) {
	res := tapOver(t, NewTap(0, nil), `{"object":"list","data":[],"model":"m"}`)
	if res.UsageKnown {
		t.Fatal("a reply with no usage block was recorded as a known zero, which bills as free")
	}
}

func TestAnEmbeddingsReplyLargerThanTheRetainCapIsNotReadAsFree(t *testing.T) {
	// Truncation must mark the usage unknown rather than leave it zero. A
	// truncated body that parses is the one case where "no usage" and "zero
	// tokens" are indistinguishable, and it must resolve to the former.
	big := `{"object":"list","data":[{"embedding":[` +
		strings.Repeat("0.123456789,", 400) + `0.5]}],"usage":{"prompt_tokens":37,"total_tokens":37}}`
	tap := NewTap(64, nil)
	res := tapOver(t, tap, big)
	if res.UsageKnown {
		t.Fatal("a truncated body was read as a complete one")
	}
}
