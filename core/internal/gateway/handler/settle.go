package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Settlement: what a finished request cost, and where that number is recorded.
//
// Separate from chat.go because this is the part with a failure policy, and
// that policy is the opposite of everything before it. Every earlier step can
// still refuse a request; from here the request has been answered and Fleet is
// the only party that can be harmed by failing.

// settleTimeout bounds the work done after the response is on the wire.
//
// The client's answer is already written, so this is Fleet's own latency to
// absorb. Five seconds is long enough for a healthy database and short enough
// that a stalled one does not hold the connection open — a gateway that pinned
// every connection on a dead database would fail far more traffic than the one
// request whose record is being written.
const settleTimeout = 5 * time.Second

// settle releases the reservation, prices the request, and records it.
//
// The order is not incidental:
//
//  1. The limiter settles first. It is the only step holding a lock, and a
//     tenant's quota is released the moment its request is done — a slow price
//     lookup must not make the tenant wait before it can spend again.
//  2. Charging is second, from the engine's own usage. P6: the engine is the
//     only authority on tokens, so a price lookup that fails cannot change
//     what was spent, only what it is worth.
//  3. Recording is last, because it is the only step that can fail in a way
//     the client would ever learn about — and by then there is nothing left to
//     fail for.
//
// Everything is passed in rather than read off the handler. A handler serves
// every request concurrently, so stashing the result on a field between
// forward and settle would be a data race, and one that only appears under
// load and only corrupts somebody's bill.
func (h *Chat) settle(r *http.Request, reservation ratelimit.Reservation, ep engine.Endpoint,
	result transport.Result, promptTokens, maxOut int, streamed bool) {

	actual := promptTokens + maxOut
	if result.UsageKnown && result.Usage != nil {
		actual = result.Usage.TotalTokens
	}
	h.Limiter.Settle(r.Context(), reservation, actual)

	rec := billing.Record{
		Model:      ep.Model,
		Endpoint:   ep.ID,
		UsageKnown: result.UsageKnown,
		TTFT:       result.TTFT,
		Duration:   result.Duration,
		Streamed:   streamed,
		OccurredAt: time.Now(),
	}
	if p, ok := authn.FromContext(r.Context()); ok {
		rec.Tenant, rec.Project, rec.KeyID = p.Tenant, p.Project, p.KeyID
	}
	if result.UsageKnown && result.Usage != nil {
		rec.Usage = *result.Usage
	} else {
		// No usage from the engine. The record still has to exist, marked as an
		// estimate, because the tokens were really spent; charging it at
		// max_tokens is P6's fallback and reconciliation finds it later. The
		// prompt count is the gateway's own estimate and is labelled as one by
		// UsageKnown being false.
		rec.Usage = openai.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: maxOut,
			TotalTokens:      actual,
		}
	}

	if h.Recorder == nil {
		return
	}
	h.record(r, rec)
}

// record prices and stores one settled request.
//
// The context is detached from the request on purpose: a client that
// disconnects mid-stream must not cancel the write of a request that was
// billed. The work is Fleet's, and the tokens were spent whether or not the
// caller stayed to hear the answer.
func (h *Chat) record(r *http.Request, rec billing.Record) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
	defer cancel()

	if h.Pricer != nil {
		amount, err := h.Pricer.Charge(ctx, rec.Model, rec.Usage)
		if err != nil {
			// An unpriced model is the operator's gap, not the tenant's fault.
			// The request is still recorded — at zero — so the tokens are not
			// lost, and the gap is logged loudly, because a model serving
			// traffic with no price is a customer being given a GPU for free and
			// nothing else in the system would say so.
			h.Log.Error("no price for the model this request used",
				"model", rec.Model, "endpoint", rec.Endpoint, "err", err)
		} else {
			rec.Amount = amount
			rec.PriceBook = h.Pricer.BookID(rec.Model)
		}
	}

	if _, err := h.Recorder.Record(ctx, rec); err != nil {
		// The one place Fleet loses money by failing. Logged with every
		// dimension needed to reconstruct the row, because a record that
		// failed to write is recoverable only by someone who can tell which
		// request it was.
		h.Log.Error("the ledger write failed; this request is unrecorded and unbilled",
			"tenant", rec.Tenant, "project", rec.Project, "key", rec.KeyID,
			"model", rec.Model, "endpoint", rec.Endpoint,
			"total_tokens", rec.Usage.TotalTokens,
			"amount_micro", int64(rec.Amount), "err", err)
	}
}
