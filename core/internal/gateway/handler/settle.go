package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
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

// settleContext is the context for everything that happens after the response.
//
// Detached from the request on purpose: a client that disconnects mid-stream
// must not cancel the write of a request that was billed. The work is Fleet's,
// and the tokens were spent whether or not the caller stayed to hear the
// answer.
func settleContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
}

// settle releases both reservations, prices the request, and writes the ledger.
//
// The rate limiter settles first: it is in memory and holds a lock, and the
// tenant's allowance should come back before a price lookup runs. The budget
// settles immediately after, because by this point the amount is known and
// leaving a reservation outstanding would spend money that was never used.
func (h *Chat) settle(r *http.Request, reservation ratelimit.Reservation, booking quota.Reservation,
	ep engine.Endpoint, result transport.Result, promptTokens, maxOut int, streamed bool) {

	actual := promptTokens + maxOut
	if result.UsageKnown && result.Usage != nil {
		actual = result.Usage.TotalTokens
	}
	h.Limiter.Settle(r.Context(), reservation, actual)

	// A record is only built when something will store it, but the charge is
	// needed either way, so it is computed once and shared.
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
		// The ledger records the project *id*, not its bare name, so that
		// usage_events.project_id and api_keys.project_id mean the same thing
		// and the two tables can be joined on it. The bare name is what the
		// limiter and the budget key on, and those are queries over
		// projects.name rather than stored columns.
		//
		// Recording the name instead was not a harmless spelling difference:
		// deleting a project counts usage_events by project id, so the check
		// that refuses to erase a project's ledger never found anything.
		rec.Tenant, rec.Project, rec.KeyID = p.Tenant, p.Project, p.KeyID
		if p.Project != "" {
			rec.Project = p.Tenant + "/" + p.Project
		}
	}
	if result.UsageKnown && result.Usage != nil {
		rec.Usage = *result.Usage
	} else {
		// No usage from the engine. The record still has to exist, marked as an
		// estimate, because the tokens were really spent; charging it at
		// max_tokens is P6's fallback and reconciliation finds it later.
		rec.Usage = openai.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: maxOut,
			TotalTokens:      actual,
		}
	}

	settleCtx, cancel := settleContext(r)
	defer cancel()

	if h.Pricer != nil {
		if amount, err := h.Pricer.Charge(settleCtx, rec.Model, rec.Usage); err != nil {
			// An unpriced model is the operator's gap, not the tenant's fault.
			// The request is still recorded — at zero — so the tokens are not
			// lost, and the gap is logged loudly, because a model serving
			// traffic with no price is a customer being given a GPU for free
			// and nothing else in the system would say so.
			h.Log.Error("no price for the model this request used",
				"model", rec.Model, "endpoint", rec.Endpoint, "err", err)
		} else {
			rec.Amount = amount
			rec.PriceBook = h.Pricer.BookID(rec.Model)
		}
	}

	// The budget settles with the same figure the ledger records, so the
	// budget and the invoice can never disagree about a request.
	if h.Budget != nil {
		h.Budget.Settle(settleCtx, booking, quota.Estimate{
			Usage:  rec.Usage,
			Amount: rec.Amount,
			Known:  rec.UsageKnown,
		})
	}

	if h.Recorder == nil {
		return
	}
	h.record(settleCtx, rec)
}

// record writes the ledger row.
//
// The context is detached from the request on purpose: a client that
// disconnects mid-stream must not cancel the write of a request that was
// billed. The work is Fleet's, and the tokens were spent whether or not the
// caller stayed to hear the answer.
func (h *Chat) record(ctx context.Context, rec billing.Record) {
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

// outOfBudget answers a refused budget.
//
// 402 rather than 429. A rate limit says "come back in a moment"; a spent budget
// says "come back after the window, or buy more", and a client that retries a
// 402 in a tight loop is behaving wrongly — each retry is a request that costs
// the platform a scheduling decision and, if it got through, a GPU. The
// Retry-After carries the window's end either way, because a client that waits
// for it is behaving correctly.
//
// The error is re-wrapped rather than passed through because the refusal came
// out of the store, and its message is prose meant for a log. What the client
// gets is the same prose with a code it can switch on.
func outOfBudget(w http.ResponseWriter, err error) {
	if e := quota.AsExceeded(err); e != nil {
		if after := e.RetryAfter(); after != "" {
			w.Header().Set("Retry-After", after)
		}
		openai.WriteError(w, errs.BudgetExhausted("%s", err.Error()))
		return
	}
	// Not a refusal — the budget store itself failed. That is an outage, not a
	// tenant problem, and answering 402 would tell a paying customer to go buy
	// more credit for a database that is merely unreachable.
	openai.WriteError(w, errs.Unavailable("the budget store is unavailable: %s", err.Error()))
}
