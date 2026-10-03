package handler

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/routing"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// Embeddings serves POST /v1/embeddings.
//
// The same lifecycle as chat — reserve, pick, forward, settle — with three
// differences that are all consequences of one fact: an embedding request
// produces no completion.
//
//   - Nothing is reserved for output, because there is no output. Reserving the
//     default completion budget anyway would let a single enormous input pass a
//     limit that a thousand small ones cannot.
//   - It is never streamed. OpenAI has no streaming embeddings, so the tap reads
//     one JSON body and there is no frame boundary to get wrong.
//   - Its prefix is the head of the first input, not a conversation. Embeddings
//     have no system prompt and no few-shot examples; the closest stable key to
//     the KV cache is the document that will be asked for again.
type Embeddings struct {
	Picker   routing.Picker
	Proxy    *transport.Proxy
	Tokens   tokenizer.Resolver
	MaxBytes int64
	// PrefixRunes bounds the hashable head of the first input.
	PrefixRunes int
	Log         *slog.Logger

	*settler
}

// NewEmbeddings builds the handler over the same billing pieces the chat
// handler uses, so a deployment cannot end up billing one and forgetting the
// other.
func NewEmbeddings(p routing.Picker, proxy *transport.Proxy, tokens tokenizer.Resolver,
	log *slog.Logger, opts ChatOptions) *Embeddings {

	if opts.Limiter == nil {
		opts.Limiter = ratelimit.NewMemory(nil)
	}
	return &Embeddings{
		Picker:      p,
		Proxy:       proxy,
		Tokens:      tokens,
		MaxBytes:    opts.MaxBytes,
		PrefixRunes: opts.PrefixRunes,
		Log:         log,
		settler:     newSettler(opts, log),
	}
}

func (h *Embeddings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBytes))
	if err != nil {
		h.refuse(w, ReasonBodyTooLarge, errs.InvalidArgument("request body unreadable or over the size limit"))
		return
	}

	req, err := openai.DecodeEmbeddingRequest(body)
	if err != nil {
		h.refuse(w, ReasonBadRequest, errs.InvalidArgument("body is not a valid embeddings request: %s", err))
		return
	}

	// Counted here rather than forwarded and discovered later, because the
	// reservation has to exist before anything is scheduled (P5) and because a
	// token-array input cannot be counted from a string at all.
	count := h.Tokens.Resolve(req.Model, "").Count
	promptTokens, err := req.PromptTokens(count)
	if err != nil {
		h.refuse(w, ReasonBadRequest, errs.InvalidArgument("%s", err))
		return
	}
	if promptTokens <= 0 {
		// A zero-token request still costs a forward pass on the engine.
		// Reserving zero reserves nothing, so a caller could send a million
		// empty inputs and pass any limit.
		h.refuse(w, ReasonBadRequest, errs.InvalidArgument("input has no tokens to charge for"))
		return
	}

	tenant, project := authn.ScopeFromContext(r.Context())
	reservation, err := h.Limiter.Reserve(r.Context(), ratelimit.Request{
		Scope:  ratelimit.Scope{Tenant: tenant, Project: project},
		Tokens: promptTokens,
	})
	if err != nil {
		h.refuse(w, ReasonRateLimited, err)
		return
	}

	ep, err := h.Picker.Pick(req.Model, req.Prefix(h.PrefixRunes))
	if err != nil {
		// Settled on the way out, as in chat: the reservation was taken and
		// nothing will return it.
		h.Limiter.Settle(r.Context(), reservation, 0)
		h.refuse(w, ReasonNoEndpoint, err)
		return
	}

	booking, err := h.reserve(r.Context(), tenant, project, ep.Model, promptTokens, 0)
	if err != nil {
		h.Limiter.Settle(r.Context(), reservation, 0)
		if h.Observed != nil {
			h.Observed.Refused(ReasonBudget)
		}
		outOfBudget(w, err)
		return
	}

	tap := transport.NewTap(0, nil)

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.Proxy.Serve(w, r, ep, tap)

	result := tap.Result()
	// Zero completion tokens: an embedding reserves exactly its input, and
	// over-reserving here would be refunded anyway while making the limiter's
	// view of a batch look larger than it is.
	h.settle(r, reservation, booking, ep, result, promptTokens, 0, false)

	h.Log.Debug("embeddings",
		"model", req.Model, "endpoint", ep.ID, "tenant", tenant, "project", project,
		"prompt_tokens", promptTokens,
		"usage_known", result.UsageKnown, "duration_ms", result.Duration.Milliseconds())
}
