package handler

import (
	"context"
	"errors"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
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
// package, which is every gateway with no database — so the settle path does
// not have to branch on whether budgeting is switched on.
func (h *settler) reserve(ctx context.Context, tenant, project, model string, promptTokens, maxOut int) (quota.Reservation, error) {
	if h.Budget == nil {
		return quota.Reservation{}, nil
	}
	est, err := h.predict(ctx, model, promptTokens, maxOut)
	if err != nil {
		// No price book at all. Not a refusal: with nothing priced, nothing is
		// being spent in units either, and a token rule is still enforceable
		// because tokens do not need a price to be counted. An empty estimate
		// reserves nothing, which is correct for units and wrong for tokens —
		// so the refusal is not silent.
		h.Log.Warn("no price book loaded; budget will be enforced on tokens only",
			"model", model, "err", err)
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
		h.Log.Warn("no price for the model; reserving at the floor rate rather than nothing",
			"model", model, "amount_micro", int64(est.Amount))
	}
	return h.Budget.Reserve(ctx, quota.Request{
		Scope:    ratelimit.Scope{Tenant: tenant, Project: project},
		Estimate: quota.Estimate{Usage: est.Usage, Amount: est.Amount, Known: est.Known},
		Now:      time.Now(),
	})
}

// predict asks the price source for this request's worst case, in tokens and
// money together.
func (h *settler) predict(ctx context.Context, model string, promptTokens, maxOut int) (billing.Prediction, error) {
	if h.Pricer == nil {
		return billing.Prediction{}, errNoPricer
	}
	return h.Pricer.Predict(ctx, model, promptTokens, maxOut)
}

// errNoPricer is the "nothing to price with" case. It is not a tenant's fault
// and must not be reported as one.
var errNoPricer = errs.Internal(errors.New("no price book is configured"))
