package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// Settlement: what a finished request cost, and where that number is recorded.
//
// Separate from the handlers because this is the part with a failure policy,
// and that policy is the opposite of everything before it. Every earlier step
// can still refuse a request; from here the request has been answered and Fleet
// is the only party that can be harmed by failing.

// settler is the part of a request's life that is the same whatever was asked
// for.
//
// It is a type rather than a package of functions because every one of its
// inputs is already on the handler, and a chat and an embeddings request share
// the whole of it: same reservations, same price book, same ledger, same
// failure policy. The one thing that differs is how many completion tokens the
// request reserved, and that is a parameter rather than a second copy of this
// file.
type settler struct {
	Limiter  ratelimit.Limiter
	Pricer   billing.PricerSource
	Recorder billing.Recorder
	Budget   quota.Limiter
	Log      *slog.Logger
	// Tokens counts an answer the engine declined to report. The same
	// resolver that counted the prompt, so an estimate and a measurement come
	// from one tokenizer and cannot disagree about the same request.
	Tokens tokenizer.Resolver

	// Observed records the request into the process's own metrics. It is the
	// *only* place a request turns into a monitoring signal, which is why it
	// lives here rather than being sprinkled through the handlers: settlement
	// is the point at which the model, the tokens and the amount are all known
	// and consistent with each other.
	Observed Observer

	// Detail mirrors the record into a store shaped for reporting. It cannot
	// fail and cannot block: the ledger write above is the authoritative one,
	// and a reporting replica is not a reason to lose money.
	Detail detail.Sink
}

// Observer is the metric surface the handlers write to. Declared here rather
// than as a concrete registry so the handlers do not depend on pkg/metrics, and
// so a test can observe without a registry at all.
type Observer interface {
	Served(rec billing.Record, duration, ttft time.Duration, streamed bool)
	Refused(reason string)
}

// newSettler builds the shared half of a handler.
//
// One function rather than a struct literal in each constructor because a
// literal in two places is a wiring that can be completed in one of them. It
// was: the chat handler was left without an observer and a gateway that
// published endpoint and build metrics but no request metrics, which is not an
// error anywhere -- it looks exactly like a gateway that served no traffic.
func newSettler(opts ChatOptions, log *slog.Logger) *settler {
	return &settler{
		Limiter:  opts.Limiter,
		Pricer:   opts.Pricer,
		Recorder: opts.Recorder,
		Budget:   opts.Budget,
		Log:      log,
		Tokens:   opts.Tokens,
		Observed: opts.Observed,
	}
}

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
//
// completion is the requested maximum, and it is only used when there is
// nothing to count. See measure.
func (s *settler) settle(r *http.Request, reservation ratelimit.Reservation, booking quota.Reservation,
	ep engine.Endpoint, result transport.Result, promptTokens, completion int, streamed bool) {

	usage, source := s.measure(ep, result, promptTokens, completion)

	actual := usage.TotalTokens
	s.Limiter.Settle(r.Context(), reservation, actual)

	rec := billing.Record{
		Model: ep.Model,
		// From the endpoint, not from configuration: the endpoint is what
		// actually served the request, and a route can be repointed at a vendor
		// between the reservation and the answer. A gateway that recorded the
		// configured provider would bill a vendor request as fleet capacity —
		// allocating money that was already spent a second time.
		Provider:    providerOf(ep),
		Endpoint:    ep.ID,
		UsageKnown:  result.UsageKnown,
		UsageSource: source,
		// Only meaningful for a counted row, and only the counted branch can
		// produce it, so it is copied rather than derived.
		Truncated:  source == billing.SourceCounted && result.TextTruncated,
		Usage:      usage,
		TTFT:       result.TTFT,
		Duration:   result.Duration,
		Streamed:   streamed,
		OccurredAt: time.Now(),
		Engine:     engineTimings(result),
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
	settleCtx, cancel := settleContext(r)
	defer cancel()

	if s.Pricer != nil {
		if amount, err := s.Pricer.Charge(settleCtx, rec.Model, rec.Provider, rec.Usage); err != nil {
			// An unpriced model is the operator's gap, not the tenant's fault.
			// The request is still recorded — at zero — so the tokens are not
			// lost, and the gap is logged loudly, because a model serving
			// traffic with no price is a customer being given a GPU for free
			// and nothing else in the system would say so.
			//
			// The provider is in the message because "no price for gpt-4o" is
			// now two different problems: nobody declared the fleet's rate, or
			// nobody declared the vendor's. They are fixed by editing two
			// different rows.
			s.Log.Error("no price for the model this request used",
				"model", rec.Model, "provider", rec.Provider,
				"endpoint", rec.Endpoint, "err", err)
		} else {
			rec.Amount = amount
			rec.PriceBook = s.Pricer.BookID(rec.Model, rec.Provider)
		}
	}

	// The budget settles with the same figure the ledger records, so the
	// budget and the invoice can never disagree about a request.
	if s.Budget != nil {
		s.Budget.Settle(settleCtx, booking, quota.Estimate{
			Usage:  rec.Usage,
			Amount: rec.Amount,
			Known:  rec.UsageKnown,
		})
	}

	if s.Observed != nil {
		ttft := result.TTFT
		if !streamed {
			// A non-streamed response has one delivery event, so "time to
			// first token" is the whole request. Reported as absent rather than
			// as the duration, because the two mean different things on a
			// latency graph.
			ttft = -1
		}
		s.Observed.Served(rec, result.Duration, ttft, streamed)
	}

	if s.Recorder == nil {
		return
	}
	s.record(settleCtx, rec)
}

// record writes the ledger row and hands it to the detail mirror.
//
// The context is detached from the request on purpose: a client that
// disconnects mid-stream must not cancel the write of a request that was
// billed. The work is Fleet's, and the tokens were spent whether or not the
// caller stayed to hear the answer.
func (s *settler) record(ctx context.Context, rec billing.Record) {
	id, err := s.Recorder.Record(ctx, rec)
	if err != nil {
		// The one place Fleet loses money by failing. Logged with every
		// dimension needed to reconstruct the row, because a record that
		// failed to write is recoverable only by someone who can tell which
		// request it was.
		s.Log.Error("the ledger write failed; this request is unrecorded and unbilled",
			"tenant", rec.Tenant, "project", rec.Project, "key", rec.KeyID,
			"model", rec.Model, "endpoint", rec.Endpoint,
			"total_tokens", rec.Usage.TotalTokens,
			"amount_micro", int64(rec.Amount), "err", err)
		return
	}

	// The mirror is keyed on the ledger's row id, so it is filled in here
	// rather than in the recorder: the id is only known after the insert, and a
	// backfill needs to be able to match the two stores by identity rather than
	// by content. Two identical requests would otherwise be indistinguishable.
	rec.LedgerID = id
	s.Detail.Enqueue(rec)
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
