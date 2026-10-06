package billing

import (
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Predict is a worst case for a request that has not run yet, in tokens and
// money together.
//
// It is what the reservation path uses, so it must be an upper bound: reserving
// less than the request can cost is the same as not reserving at all.
//
// The whole prompt is charged at the fresh input rate. A cache hit is unknowable
// before the request runs — the prefix cache is inside the engine (see
// architecture §7) — so an estimate that assumed a hit would under-reserve
// exactly when caching works best.
//
// The unit is the caller's to interpret, and the two centres make that a
// distinction worth stating. A pool rate is not money per token: it weights a
// tenant's token consumption into a share of a fixed monthly pool, so the same
// number means something entirely different depending on the GPU-hour rate the
// operator declared. A direct rate is money per token, and a reserve against it
// is exact. Budgets in units therefore keep working for both — one was always a
// weighting — but only the direct figure should ever be read as a price.
func (p *Pricer) Predict(model string, provider Provider, promptTokens, maxTokens int) Prediction {
	provider = NormalizeProvider(provider)
	usage := openai.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: maxTokens,
		TotalTokens:      promptTokens + maxTokens,
	}
	rate, ok := p.book.Rate(model, provider)
	if !ok {
		return p.unpriced(model, provider, usage)
	}
	return Prediction{
		Usage:    usage,
		Amount:   units(promptTokens, rate.Input) + units(maxTokens, rate.Output),
		Model:    model,
		Provider: provider,
		Known:    true,
	}
}

// unpriced reserves against the cheapest rate in the same cost centre, and says
// it does not know.
//
// Returning zero would leave a budget unenforced for exactly the models nobody
// has priced yet, and the budget and the ledger would then agree that nothing
// was spent while describing a request that a vendor billed for.
//
// Falling back within the centre rather than across it is the load-bearing part.
// A fleet rate is a weight for dividing a pool and a vendor rate is money per
// token; the two are not on the same scale and are often three orders of
// magnitude apart. A vendor request with no price of its own, reserved against a
// fleet weight, would under-reserve by that factor and let a tenant spend a
// month's budget in an afternoon. So an unpriced vendor request falls back to the
// cheapest vendor price, and an unpriced fleet request to the cheapest fleet
// weight.
//
// Over-estimates on purpose: an unpriced model stops a tenant sooner rather than
// later, which is the direction where the mistake is recoverable — give the
// model a price and they carry on.
func (p *Pricer) unpriced(model string, provider Provider, usage openai.Usage) Prediction {
	rate, ok := p.cheapest(provider)
	if !ok {
		// Nothing is priced in this centre at all, so nothing is being spent
		// in it either. Zero is the right answer rather than a fallback: there
		// is no rate to fall back to, and borrowing another centre's would
		// invent one.
		return Prediction{Usage: usage, Model: model, Provider: provider}
	}
	// One rate for both directions, since there is no model to have separate
	// input and output rates for.
	amount := units(usage.PromptTokens+usage.CompletionTokens, rate.Input)
	return Prediction{
		Usage:    usage,
		Amount:   amount,
		Model:    model,
		Provider: provider,
		Known:    false,
	}
}

// cheapest is the lowest input rate from one provider. A false return means that
// centre has no price at all, which Predict reports as a known-zero rather than
// borrowing a number from somewhere else.
func (p *Pricer) cheapest(provider Provider) (Rate, bool) {
	want := NormalizeProvider(provider)
	var lowest Rate
	found := false
	for key, rate := range p.book.prices {
		if providerOf(key) != want {
			continue
		}
		if !found || rate.Input < lowest.Input {
			lowest, found = rate, true
		}
	}
	return lowest, found
}

// providerOf splits a book key back into its provider. The separator is a byte
// that cannot occur in a model name, so this is unambiguous rather than a
// strings.Cut guess.
func providerOf(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == 0 {
			return key[i+1:]
		}
	}
	return ""
}
