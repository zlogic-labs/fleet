package handler

import (
	"context"
	"errors"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The budget, and what a model with no price means to it.
//
// The reservation shape is ratelimit's, reused rather than reinvented: the
// difference between checking a budget and enforcing one is the same difference
// between reading a counter and deducting inside the read, and that argument
// was already made and tested there.

// reserve commits an upper bound on what this request may cost, in tokens and
// in money.
//
// It returns a no-op reservation when there is nothing to enforce — no budget
// package, which is every gateway with no database, or no tenant to enforce
// against — so the settle path does not have to branch on whether budgeting is
// switched on.
func (h *settler) reserve(ctx context.Context, tenant, project string, ep engine.Endpoint, promptTokens, maxOut int) (quota.Reservation, error) {
	if h.Budget == nil {
		return quota.Reservation{}, nil
	}
	// A budget is a statement about a tenant: a rule names a scope, and a
	// scope with no tenant is not a smaller budget, it is no budget. Refusing
	// here instead would make "auth off, database on" answer every request
	// with 503, which is the shape of a platform that owns GPUs and refuses to
	// run its owner's traffic.
	//
	// What this does not do is make the request free. Rate limiting, pricing
	// and the ledger all still run in settle; only the ceiling is skipped,
	// because there is nothing to write a ceiling against.
	if tenant == "" {
		return quota.Reservation{}, nil
	}
	est, err := h.predict(ctx, ep, promptTokens, maxOut)
	if err != nil {
		// No price book at all. Not a refusal: with nothing priced, nothing is
		// being spent in units either, and a token rule is still enforceable
		// because tokens do not need a price to be counted. An empty estimate
		// reserves nothing, which is correct for units and wrong for tokens —
		// so the refusal is not silent.
		h.Log.Warn("no price book loaded; budget will be enforced on tokens only",
			"model", ep.Model, "err", err)
		return h.Budget.Reserve(ctx, quota.Request{
			Scope:    ratelimit.Scope{Tenant: tenant, Project: project},
			Estimate: quota.Estimate{Usage: openai.Usage{TotalTokens: promptTokens + maxOut}},
			Now:      time.Now(),
		})
	}
	if !est.Known {
		// The floor-rate fallback is deliberately an over-estimate, so this
		// stops the tenant sooner than it should rather than later. Logged
		// because the operator, not the tenant, is the one who has to fix it.
		// The floor rate comes from the same cost centre as the request, so a
		// vendor request without a price is reserved against the cheapest
		// vendor price and never against the fleet's weighting — those are not
		// on the same scale, and borrowing across that gap under-reserves by
		// orders of magnitude. See billing.Pricer.Predict.
		h.Log.Warn("no price for the model; reserving at the floor rate rather than nothing",
			"model", ep.Model, "provider", providerOf(ep), "amount_micro", int64(est.Amount))
	}
	return h.Budget.Reserve(ctx, quota.Request{
		Scope:    ratelimit.Scope{Tenant: tenant, Project: project},
		Estimate: quota.Estimate{Usage: est.Usage, Amount: est.Amount, Known: est.Known},
		Now:      time.Now(),
	})
}

// predict asks the price source for this request's worst case, in tokens and
// money together.
//
// The endpoint rather than a model name, because the provider is half of what a
// price is and only the endpoint knows which one answered. Reserving at the
// fleet's rate for a request that will go to a vendor under-reserves by the
// ratio between a pool weighting and a price per token.
func (h *settler) predict(ctx context.Context, ep engine.Endpoint, promptTokens, maxOut int) (billing.Prediction, error) {
	if h.Pricer == nil {
		return billing.Prediction{}, errNoPricer
	}
	return h.Pricer.Predict(ctx, ep.Model, providerOf(ep), promptTokens, maxOut)
}

// errNoPricer is the "nothing to price with" case. It is not a tenant's fault
// and must not be reported as one.
var errNoPricer = errs.Internal(errors.New("no price book is configured"))
