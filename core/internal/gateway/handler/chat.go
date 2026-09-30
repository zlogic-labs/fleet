// Package handler implements the OpenAI-compatible endpoints.
package handler

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/routing"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// Chat serves POST /v1/chat/completions.
type Chat struct {
	Picker   routing.Picker
	Proxy    *transport.Proxy
	Tokens   tokenizer.Resolver
	Log      *slog.Logger
	MaxBytes int64
	// PrefixRunes bounds the hashable prompt prefix.
	PrefixRunes int

	samples *SampleLog
}

// ChatOptions groups the tunables the server reads from config.
type ChatOptions struct {
	MaxBytes     int64
	PrefixRunes  int
	SampleBuffer int
}

// NewChat builds the handler and its rolling sample log, which the Fleet
// status endpoint reads.
func NewChat(p routing.Picker, proxy *transport.Proxy, tokens tokenizer.Resolver, log *slog.Logger, opts ChatOptions) *Chat {
	if opts.SampleBuffer <= 0 {
		opts.SampleBuffer = 32
	}
	return &Chat{
		Picker:      p,
		Proxy:       proxy,
		Tokens:      tokens,
		Log:         log,
		MaxBytes:    opts.MaxBytes,
		PrefixRunes: opts.PrefixRunes,
		samples:     NewSampleLog(opts.SampleBuffer),
	}
}

func (h *Chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBytes))
	if err != nil {
		h.fail(w, r, errs.InvalidArgument("request body unreadable or over the size limit"))
		return
	}

	req, err := openai.DecodeChatRequest(body)
	if err != nil {
		h.fail(w, r, errs.InvalidArgument("body is not a valid chat completion request: %s", err))
		return
	}

	// The limiter needs a prompt estimate before anything is forwarded (P5).
	// Counting here rather than in a middleware keeps the tokenizer on the
	// same code path as the routing hash, one parse of the body.
	promptTokens := h.Tokens.Resolve(req.Model, "").Count(req.Prefix(1 << 16))

	ep, err := h.Picker.Pick(req.Model, req.Prefix(h.PrefixRunes))
	if err != nil {
		h.fail(w, r, err)
		return
	}

	tap := transport.NewTap(0, nil)
	h.forward(w, r, ep, body, req, tap)

	result := tap.Result()
	h.samples.Add(Sample{
		Model:      req.Model,
		Endpoint:   ep.ID,
		TTFT:       result.TTFT,
		Duration:   result.Duration,
		Bytes:      result.Bytes,
		Usage:      result.Usage,
		UsageKnown: result.UsageKnown,
		PromptEst:  promptTokens,
		Requested:  req.ResolveMaxTokens(0),
		Streamed:   req.Stream,
	})
	h.Log.Debug("chat completion",
		"model", req.Model, "endpoint", ep.ID,
		"ttft_ms", result.TTFT.Milliseconds(), "duration_ms", result.Duration.Milliseconds(),
		"usage_known", result.UsageKnown, "prompt_est", promptTokens)
}

// forward rewrites the body for billing and hands it to the proxy. The tap has
// to outlive ServeHTTP, so the response is fully written when Serve returns.
func (h *Chat) forward(w http.ResponseWriter, r *http.Request, ep engine.Endpoint, body []byte, req *openai.ChatRequest, tap *transport.Tap) {
	if req.Stream {
		body = ensureIncludeUsage(body)
	}
	// The body is replaced unconditionally. It was drained to count tokens and
	// compute the routing hash, and forwarding a drained body with the original
	// ContentLength makes the transport fail before the request is sent.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.Proxy.Serve(w, r, ep, tap)
}

func (h *Chat) fail(w http.ResponseWriter, _ *http.Request, err error) {
	openai.WriteError(w, err)
}

// Samples exposes the rolling log for the status endpoint.
func (h *Chat) Samples() []Sample { return h.samples.Snapshot() }

// ── stream_options injection ───────────────────────────────────

// includeUsageField is what Fleet inserts so that a streamed completion
// carries token counts at all. Without it the engine sends deltas only, the
// response is unbillable, and P6 forces the gateway to charge max_tokens —
// which is worse for the customer than Fleet setting the flag itself.
const includeUsageField = `"stream_options":{"include_usage":true}`

// ensureIncludeUsage returns body with stream_options.include_usage set.
//
// The common case is a surgical byte edit: a well-formed JSON object's last
// byte is its closing brace, so inserting before it costs nothing and keeps
// every other byte — including fields this struct does not model — intact.
// Re-marshalling instead would reorder keys and can render numbers
// differently, which is a poor trade for a gateway.
func ensureIncludeUsage(body []byte) []byte {
	if bytes.Contains(body, []byte(`"include_usage":true`)) {
		return body
	}
	trimmed := bytes.TrimRight(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return body
	}
	inner := bytes.TrimRight(trimmed[:len(trimmed)-1], " \t\r\n")

	out := make([]byte, 0, len(trimmed)+len(includeUsageField)+1)
	if len(inner) == 0 {
		out = append(out, '{')
	} else {
		out = append(out, inner...)
		out = append(out, ',')
	}
	out = append(out, includeUsageField...)
	out = append(out, '}')
	return out
}

// ── rolling sample log ────────────────────────────────────────

// Sample is one completed request, as the console shows it.
type Sample struct {
	Model      string
	Endpoint   string
	TTFT       time.Duration
	Duration   time.Duration
	Bytes      int64
	Usage      *openai.Usage
	UsageKnown bool
	PromptEst  int
	Requested  int
	Streamed   bool
}

type SampleLog struct {
	mu      sync.RWMutex
	samples []Sample
	limit   int
}

func NewSampleLog(limit int) *SampleLog {
	return &SampleLog{limit: limit}
}

func (l *SampleLog) Add(s Sample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples = append(l.samples, s)
	if len(l.samples) > l.limit {
		l.samples = l.samples[len(l.samples)-l.limit:]
	}
}

func (l *SampleLog) Snapshot() []Sample {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Sample(nil), l.samples...)
}
