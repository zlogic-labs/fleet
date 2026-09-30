package transport_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

const usageFrame = `data: {"id":"1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

const deltaFrame = `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"}}]}`

// A frame split across TCP segments is the classic way to lose a usage chunk.
// Feeding the stream one byte at a time reproduces it exactly.
func TestTapFindsUsageInSSEFramesSplitAcrossReads(t *testing.T) {
	stream := deltaFrame + "\n\n" + usageFrame + "\n\n" + "data: [DONE]\n\n"

	tap := transport.NewTap(0, nil)
	tap.UseSSE(true)

	// One byte per Read, so every frame boundary lands mid-slice.
	reader := tap.Reader(nopCloser{iotest.OneByteReader(strings.NewReader(stream))})
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("copy: %v", err)
	}

	res := tap.Result()
	if !res.UsageKnown {
		t.Fatal("UsageKnown = false, want the final usage chunk to be seen")
	}
	if res.Usage.PromptTokens != 11 || res.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %+v, want prompt=11 completion=7", res.Usage)
	}
	if res.Bytes != int64(len(stream)) {
		t.Errorf("Bytes = %d, want %d (tap must be transparent)", res.Bytes, len(stream))
	}
}

// An engine that ignores stream_options leaves the response unbillable. The
// tap must say so rather than report a zero-token success.
func TestTapReportsMissingUsage(t *testing.T) {
	tap := transport.NewTap(0, nil)
	tap.UseSSE(true)
	body := tap.Reader(nopCloser{strings.NewReader(deltaFrame + "\n\n")})
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatalf("copy: %v", err)
	}

	if res := tap.Result(); res.UsageKnown || res.Usage != nil {
		t.Fatalf("Result = %+v, want UsageKnown=false so billing applies the P6 cap", res)
	}
}

func TestTapParsesNonStreamUsage(t *testing.T) {
	body := `{"id":"1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9,` +
		`"total_tokens":14,"prompt_tokens_details":{"cached_tokens":2}}}`

	tap := transport.NewTap(0, nil)
	if _, err := io.Copy(io.Discard, tap.Reader(nopCloser{strings.NewReader(body)})); err != nil {
		t.Fatalf("copy: %v", err)
	}

	res := tap.Result()
	if !res.UsageKnown {
		t.Fatal("UsageKnown = false for a non-streaming body carrying usage")
	}
	if got := res.Usage.FreshPromptTokens(); got != 3 {
		t.Errorf("FreshPromptTokens() = %d, want 3 (5 prompt minus 2 cached)", got)
	}
}

func TestProxyStreamsAndTapsThroughToClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("outbound Accept-Encoding = %q, want identity so frames stay readable", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, deltaFrame+"\n\n"+usageFrame+"\n\n")
	}))
	defer upstream.Close()

	tap := transport.NewTap(0, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))

	transport.New(transport.Options{}).Serve(rec, req, engine.Endpoint{BaseURL: upstream.URL}, tap)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), usageFrame) {
		t.Error("client did not receive the usage frame; the tap is not transparent")
	}
	if res := tap.Result(); !res.UsageKnown || res.Usage.PromptTokens != 11 {
		t.Fatalf("tap result = %+v, want the engine usage", res)
	}
}

// A 4xx is the caller's own error; rewriting it would hide the engine's
// explanation of what was wrong with the request.
func TestProxyPassesClientErrorsThroughVerbatim(t *testing.T) {
	original := `{"error":{"message":"context length exceeded","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, original)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	transport.New(transport.Options{}).Serve(rec, req, engine.Endpoint{BaseURL: upstream.URL}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if rec.Body.String() != original {
		t.Errorf("body = %s, want the engine's error verbatim", rec.Body.String())
	}
}

// A 5xx with an HTML body is the common failure of something in between. The
// client must still receive parseable JSON with a retryable status.
func TestProxyRewritesServerErrorsIntoOpenAIEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	transport.New(transport.Options{}).Serve(rec, req, engine.Endpoint{BaseURL: upstream.URL}, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so a client can retry another replica", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var env openai.ErrorEnvelope
	if err := decodeJSON(rec.Body.String(), &env); err != nil {
		t.Fatalf("response is not a valid OpenAI error envelope: %v", err)
	}
	if env.Error.Message == "" || env.Error.Type != "server_error" {
		t.Errorf("envelope = %+v, want a populated server_error", env.Error)
	}
}

// An unreachable engine must still produce a parseable error rather than Go's
// default plain-text 502 page.
func TestProxyReportsUnreachableEngine(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	// Port 0 on the loopback interface refuses connections deterministically.
	transport.New(transport.Options{}).Serve(rec, req, engine.Endpoint{BaseURL: "http://127.0.0.1:0"}, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var env openai.ErrorEnvelope
	if err := decodeJSON(rec.Body.String(), &env); err != nil {
		t.Fatalf("response is not a valid OpenAI error envelope: %v", err)
	}
}

type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func decodeJSON(s string, v any) error { return json.Unmarshal([]byte(s), v) }
