// Package handler implements the OpenAI-compatible endpoints.
package handler

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/routing"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
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
	Limiter  ratelimit.Limiter
	Log      *slog.Logger
	MaxBytes int64
	// PrefixRunes bounds the hashable prompt prefix.
	PrefixRunes int
	// DefaultMaxTokens is what a request that omits max_tokens reserves for.
	//
	// It has to exist because reserving nothing for an unbounded request would
	// let one call with no max_tokens pass a token limit that a hundred
	// bounded calls cannot. It is the server's answer to "how much may one
	// request cost", which is the same question a quota answers per tenant.
	DefaultMaxTokens int

	samples *SampleLog
}

// ChatOptions groups the tunables the server reads from config.
type ChatOptions struct {
	MaxBytes         int64
	PrefixRunes      int
	SampleBuffer     int
	DefaultMaxTokens int
	Limiter          ratelimit.Limiter
}

// NewChat builds the handler and its rolling sample log, which the Fleet
// status endpoint reads.
func NewChat(p routing.Picker, proxy *transport.Proxy, tokens tokenizer.Resolver, log *slog.Logger, opts ChatOptions) *Chat {
	if opts.SampleBuffer <= 0 {
		opts.SampleBuffer = 32
	}
	if opts.DefaultMaxTokens <= 0 {
		opts.DefaultMaxTokens = 1024
	}
	if opts.Limiter == nil {
		// An unlimited limiter rather than a nil check on every request: the
		// handler's hot path should not branch on whether the deployment
		// configured a limit.
		opts.Limiter = ratelimit.NewMemory(nil)
	}
	return &Chat{
		Picker:           p,
		Proxy:            proxy,
		Tokens:           tokens,
		Limiter:          opts.Limiter,
		Log:              log,
		MaxBytes:         opts.MaxBytes,
		PrefixRunes:      opts.PrefixRunes,
		DefaultMaxTokens: opts.DefaultMaxTokens,
		samples:          NewSampleLog(opts.SampleBuffer),
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

	// Reserve before picking an endpoint, so a tenant over its limit is turned
	// away without having caused a scheduling decision or a connection. The
	// reservation is the request's upper bound: prompt estimate plus the
	// requested maximum completion.
	//
	// ReserveMaxTokens matters here. A client that omits max_tokens has asked
	// for the whole context window, and reserving a small default for it would
	// let one unbounded request pass a limit that a thousand small ones
	// cannot.
	tenant := authn.TenantFromContext(r.Context())
	maxOut := req.ResolveMaxTokens(0)
	if maxOut <= 0 {
		maxOut = h.DefaultMaxTokens
	}
	reservation, err := h.Limiter.Reserve(r.Context(), ratelimit.Request{
		Tenant: tenant,
		Tokens: promptTokens + maxOut,
	})
	if err != nil {
		rateLimited(w, err)
		return
	}

	ep, err := h.Picker.Pick(req.Model, req.Prefix(h.PrefixRunes))
	if err != nil {
		// Settled on the way out even though nothing was generated: the
		// reservation was taken and nothing will return it. Forgetting this
		// leaks the tenant's whole limit into every 404.
		h.Limiter.Settle(r.Context(), reservation, 0)
		h.fail(w, r, err)
		return
	}

	tap := transport.NewTap(0, nil)
	h.forward(w, r, ep, body, req, tap)

	result := tap.Result()

	// Settle against what the engine reported, not what was asked for. P6:
	// the engine's usage is the only authority, so a client claiming fewer
	// tokens changes nothing, and a client that asked for 4096 and got 7 is
	// refunded for the 4089 it did not use.
	actual := promptTokens + maxOut
	if result.UsageKnown && result.Usage != nil {
		actual = result.Usage.TotalTokens
	}
	h.Limiter.Settle(r.Context(), reservation, actual)
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

// rateLimited answers a refused reservation.
//
// Retry-After is set from the limiter's own message rather than by recomputing
// the window here: two places computing the reset time will eventually
// disagree, and the one that disagrees is the one a client retries against.
func rateLimited(w http.ResponseWriter, err error) {
	if secs := ratelimit.RetryAfterSeconds(err); secs != "" {
		w.Header().Set("Retry-After", secs)
	}
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
