package billing

import (
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

func (p *Pricer) Estimate(model string, promptTokens, maxTokens int) Amount {
	rate, ok := p.book.Rate(model)
	if !ok {
		return 0
	}
	// The whole prompt is charged at the fresh input rate. A cache hit is
	// unknowable before the request runs — the prefix cache is inside the engine
	// (see architecture §7) — so an estimate that assumed a hit would
	// under-reserve exactly when caching works best.
	return units(promptTokens, rate.Input) + units(maxTokens, rate.Output)
}

// Predict is a worst case for a request that has not run yet, in tokens and
// money together.
//
// It is what the reservation path uses, so it must be an upper bound: reserving
// less than the request can cost is the same as not reserving at all. The prompt
// count passed in is the gateway's own estimate, and maxTokens is the client's
// ceiling.
//
// The whole prompt is charged at the fresh input rate. A cache hit is unknowable
// before the request runs — the prefix cache is inside the engine (see
// architecture §7) — so an estimate that assumed a hit would under-reserve
// exactly when caching works best.
//
// A model with no price falls back to the cheapest rate in force, and reports
// Known=false. Returning zero here would leave a budget unenforced for exactly
// the models nobody has priced yet, and the budget and the ledger would then
// agree that nothing was spent while describing a request that used GPU time.
// The fallback over-estimates on purpose: an unpriced model stops a tenant
// sooner rather than later, which is the direction where the mistake is
// recoverable — give the model a price and they carry on.
func (p *Pricer) Predict(model string, promptTokens, maxTokens int) Prediction {
	rate, ok := p.book.Rate(model)
	if !ok {
		rate = p.cheapest()
		if len(p.book.prices) == 0 {
			// Nothing is priced at all, so nothing is being spent in units
			// either. Zero is the right answer here rather than a fallback:
			// there is no rate to fall back to.
			return Prediction{
				Usage:  openai.Usage{PromptTokens: promptTokens, CompletionTokens: maxTokens, TotalTokens: promptTokens + maxTokens},
				Model:  model,
				Amount: 0,
			}
		}
		// One rate for both directions, since there is no model to have
		// separate input and output rates for.
		amount := units(promptTokens+maxTokens, rate.Input)
		return Prediction{
			Usage: openai.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: maxTokens,
				TotalTokens:      promptTokens + maxTokens,
			},
			Amount: amount,
			Model:  model,
			Known:  false,
		}
	}
	return Prediction{
		Usage: openai.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: maxTokens,
			TotalTokens:      promptTokens + maxTokens,
		},
		Amount: units(promptTokens, rate.Input) + units(maxTokens, rate.Output),
		Model:  model,
		Known:  true,
	}
}

// cheapest is the lowest input rate in the book. A zero Rate means the book is
// empty, which Predict reports as a known-zero rather than a free request.
func (p *Pricer) cheapest() Rate {
	if len(p.book.prices) == 0 {
		return Rate{}
	}
	lowest := p.book.prices[p.book.Models()[0]]
	for _, m := range p.book.Models() {
		if r := p.book.prices[m]; r.Input < lowest.Input {
			lowest = r
		}
	}
	return lowest
}
