package handler

import (
	"net/http"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The refusals the gateway makes after a request has been accepted, and the
// metric reason each one is counted under. They live apart from settle.go
// because a refusal is not a settlement: nothing was spent, and conflating the
// two would put requests that were never sent into the spend counters.
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

// Refusal reasons.
//
// A closed set, so that a reason is never an error message. Grouping a metric
// by message produces a series per distinct wording, and the wording changes
// whenever someone edits a sentence.
const (
	ReasonRateLimited  = "rate_limited"
	ReasonBudget       = "budget_exhausted"
	ReasonNoEndpoint   = "no_endpoint"
	ReasonBadRequest   = "bad_request"
	ReasonBodyTooLarge = "body_too_large"
)

// refuse records and answers a turn-away in one step.
//
// Counting at the answer rather than at the cause is deliberate: the cause can
// be reached down several paths, and a counter incremented per path is a
// counter that is under the count somewhere nobody thought of.
func (s *settler) refuse(w http.ResponseWriter, reason string, err error) {
	if s.Observed != nil {
		s.Observed.Refused(reason)
	}
	writeRefusal(w, err)
}
