// Package handler implements the OpenAI-compatible endpoints.
package handler

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/routing"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
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

	// Pricer and Recorder are the billing side of a settled request, and both
	// are nil when the deployment keeps no ledger. A gateway with neither still
	// serves traffic and still enforces limits; it just cannot say afterwards
	// what anything cost, which is a laptop, not a production shape.
	Pricer   billing.PricerSource
	Recorder billing.Recorder
	// Budget is nil when no spend budget is enforced, which is every
	// deployment that has not set one and every gateway with no database. A
	// tenant with no budget is not refused; it is simply not capped.
	Budget quota.Limiter

	samples *SampleLog
}

// ChatOptions groups the tunables the server reads from config.
type ChatOptions struct {
	MaxBytes         int64
	PrefixRunes      int
	SampleBuffer     int
	DefaultMaxTokens int
	Limiter          ratelimit.Limiter
	Pricer           billing.PricerSource
	Recorder         billing.Recorder
	Budget           quota.Limiter
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
		Pricer:           opts.Pricer,
		Recorder:         opts.Recorder,
		Budget:           opts.Budget,
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
	//
	// Both halves of the scope come from the authenticated principal, never
	// from the request body: a client that named its own project would charge
	// whichever of the tenant's budgets it liked.
	tenant, project := authn.ScopeFromContext(r.Context())
	maxOut := req.ResolveMaxTokens(0)
	if maxOut <= 0 {
		maxOut = h.DefaultMaxTokens
	}
	reservation, err := h.Limiter.Reserve(r.Context(), ratelimit.Request{
		Scope:  ratelimit.Scope{Tenant: tenant, Project: project},
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
		// leaks the tenant's whole limit into every 404 — and with two levels
		// charged, it would leak into both the envelope and the partition.
		h.Limiter.Settle(r.Context(), reservation, 0)
		h.fail(w, r, err)
		return
	}

	// The budget is reserved here rather than beside the rate limit above, and
	// the reason is that it cannot be done earlier: pricing needs the resolved
	// model, and the model is only known once an endpoint has been picked.
	//
	// The rate limit's reservation precedes Pick so an over-limit tenant costs
	// no scheduling decision. A budget cannot have that, and the asymmetry is
	// cheap — a Pick is a hash and a health check, not a connection — whereas
	// pricing the *unresolved* name would estimate zero for anything not
	// spelled exactly like a price book entry, and reserving zero reserves
	// nothing.
	booking, err := h.reserve(r.Context(), tenant, project, ep.Model, promptTokens, maxOut)
	if err != nil {
		// The rate limit reservation is released: nothing was generated, and
		// holding it would spend a tenant's minute on a request the budget
		// refused for an unrelated reason.
		h.Limiter.Settle(r.Context(), reservation, 0)
		outOfBudget(w, err)
		return
	}

	tap := transport.NewTap(0, nil)
	h.forward(w, r, ep, body, req, tap)

	result := tap.Result()

	// Settle against what the engine reported, not what was asked for. P6: the
	// engine's usage is the only authority, so a client claiming fewer tokens
	// changes nothing, and a client that asked for 4096 and got 7 is refunded
	// for the 4089 it did not use. settle also prices the request and writes
	// the ledger; see settle.go for the order and its reasons.
	h.settle(r, reservation, booking, ep, result, promptTokens, maxOut, req.Stream)

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
