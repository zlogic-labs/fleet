package handler

import (
	"context"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The budget, and what a model with no price means to it.
//
// The reservation shape is ratelimit's, reused rather than reinvented: the
// difference between checking a budget and enforcing one is the same difference
// between reading a counter and deducting inside the read, and that argument
// was already made and tested there.

// reserve commits an upper bound on what this request may cost.
//
// It returns a no-op reservation when there is nothing to enforce — no budget
// configured, or no quota package — so the caller's settle path does not have
// to branch on whether budgeting is switched on.
func (h *Chat) reserve(ctx context.Context, tenant, project, model string, promptTokens, maxOut int) (quota.Reservation, error) {
	if h.Budget == nil {
		return quota.Reservation{}, nil
	}
	bound := h.bound(model, promptTokens, maxOut)
	return h.Budget.Reserve(ctx, quota.Request{
		Scope: ratelimit.Scope{Tenant: tenant, Project: project},
		Bound: bound,
		Now:   time.Now(),
	})
}

// bound is the most this request could cost.
//
// This is the price book's estimate at the client's ceiling, not a prediction:
// the engine may ignore max_tokens and the prompt may be longer than the
// gateway estimated, and settlement records whatever really happened. What the
// estimate has to be is an upper bound that is not absurdly high, or a tenant
// with a real budget gets refused by their own worst case.
//
// A model with no price estimates to zero, and zero is the one value that must
// not be passed through. Reserving nothing leaves the budget unenforced for
// exactly the models that are unbilled, so a tenant could run an unpriced model
// at full tilt and the budget would never notice — the budget and the ledger
// would agree that nothing was spent, and both would be describing a request
// that used GPU time.
//
// The fallback is the client's ceiling priced at the cheapest rate any model
// currently has, which is a real number and therefore refuses eventually. It
// is an over-estimate, so an unpriced model stops a tenant sooner than it
// should rather than later; that is the direction where the mistake is
// recoverable, because the tenant can be given a price and carry on.
func (h *Chat) bound(model string, promptTokens, maxOut int) billing.Amount {
	if h.Pricer == nil {
		return 0
	}
	if b := h.Pricer.Estimate(model, promptTokens, maxOut); b > 0 {
		return b
	}
	if h.Log != nil {
		h.Log.Warn("no price for the model; reserving the floor rate instead of nothing",
			"model", model, "fallback", h.floorBound(promptTokens+maxOut))
	}
	return h.floorBound(promptTokens + maxOut)
}

// floorBound prices a token count at the cheapest rate in force.
//
// It returns zero when there is no price book at all, which is the one case
// where reserving nothing is correct: with nothing priced, nothing is being
// spent in units either, and there is no budget to enforce.
func (h *Chat) floorBound(tokens int) billing.Amount {
	if h.Pricer == nil || !h.Pricer.AnyPriced() {
		return 0
	}
	return h.Pricer.EstimateAtCheapest(tokens)
}
