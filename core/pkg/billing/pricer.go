package billing

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Amount is a priced request, in millionths of a quota unit.
//
// It is an integer, not a float currency amount, for the reason in the package
// comment: the authoritative figure is derived at month end, and anything
// derived here must be exact or the ledger will not reconcile.
//
// The scale is millionths because a whole unit is far too coarse for a single
// request. At a rate of 1000 units per million tokens, a 100-token prompt is
// worth 0.1 units, and a 500-token cached prompt 0.05 — so charging in whole
// units bills *nothing* for every request under 1000 tokens, which is most of
// them. That is not a rounding nit: it is the platform giving away the most
// common request it serves. Rounding to the nearest whole unit instead would
// over-charge roughly half of them by 0.5 units, which is worse.
//
// Micro-units keep the arithmetic integral and exact while making the smallest
// plausible request cost something non-zero. The cost is a wider column, which
// is irrelevant next to the ledger rows around it.
type Amount int64

// MicroPerUnit converts micro-units to whole units, truncating.
//
// Truncation is safe *here* — once per line item, not once per token — and the
// direction is the platform's, which is stated so nobody has to reverse-engineer
// it. The ledger does not sum truncated values; it sums micro-units and converts
// once at the end, so the error cannot accumulate across millions of rows.
func (a Amount) Units() float64 { return float64(a) / MicroPerUnit }

// MicroPerUnit is the number of micro-units in one unit.
const MicroPerUnit = 1_000_000

// String renders an amount in units.
//
// Written out by hand rather than as %f of Units() because this ends up in
// error messages and API responses about money, and a float conversion of a
// large micro figure prints 1.0260000000000001 — a number that is not the
// charge, in the one place the tenant is being told what the charge is. The
// integer is divided and the remainder is carried as digits, so nothing rounds.
func (a Amount) String() string {
	if a == 0 {
		return "0"
	}
	neg := a < 0
	if neg {
		a = -a
	}
	whole, frac := a/MicroPerUnit, a%MicroPerUnit

	// Six digits is the precision a micro-unit carries, so the trailing zeros
	// are always dropped rather than always printed.
	digits := strconv.FormatInt(int64(frac), 10)
	digits = strings.Repeat("0", 6-len(digits)) + digits
	digits = strings.TrimRight(digits, "0")

	out := strconv.FormatInt(int64(whole), 10)
	if digits != "" {
		out += "." + digits
	}
	if neg {
		return "-" + out
	}
	return out
}

// Pricer prices usage.
//
// It holds the book rather than taking one per call so the hot path is a map
// lookup and a struct field, not an interface dispatch on every request.
type Pricer struct {
	book *Book
}

// NewPricer returns a pricer over the book.
func NewPricer(b *Book) *Pricer { return &Pricer{book: b} }

// Charge prices one usage record.
//
// The arithmetic is written out longhand rather than looped over a rate map
// because the two subtleties below are invisible in a loop and are exactly the
// ones that double-charge:
//
//   - prompt_tokens already INCLUDES cached_tokens, so the fresh input is the
//     difference. Charging prompt_tokens at the input rate *and* cached_tokens at
//     the cached rate bills the cached portion twice.
//   - completion_tokens already INCLUDES reasoning_tokens, so reasoning is
//     carved out of the output rather than added to it.
//
// Both inclusions are the wire format's, not a Fleet choice.
func (p *Pricer) Charge(model string, u openai.Usage) (Amount, error) {
	rate, ok := p.book.Rate(model)
	if !ok {
		return 0, fmt.Errorf("no price for model %q", model)
	}
	return charge(rate, u), nil
}

// charge is the arithmetic, separated so it can be tested and reasoned about
// without a Book.
func charge(rate Rate, u openai.Usage) Amount {
	cached := u.CachedPromptTokens()
	reasoning := u.ReasoningTokens()

	// A clamp, not a correction: an engine that reports more cached tokens than
	// it reports prompt tokens is misreporting, and subtracting would produce a
	// negative fresh count that then gets priced as a large negative credit.
	// Charging the cached portion at the cached rate and nothing at the input
	// rate is the least-wrong reading, and the ledger row keeps the raw numbers
	// so the discrepancy is visible rather than silently absorbed.
	fresh := u.PromptTokens - cached
	if fresh < 0 {
		fresh = 0
	}
	// Same for reasoning: a completion count below its own reasoning count.
	plainOutput := u.CompletionTokens - reasoning
	if plainOutput < 0 {
		plainOutput = 0
	}

	reasoningRate := rate.Reasoning
	if reasoningRate == 0 {
		reasoningRate = rate.Output
	}

	return units(fresh, rate.Input) +
		units(cached, rate.Cached) +
		units(plainOutput, rate.Output) +
		units(reasoning, reasoningRate)
}

// units converts a token count to micro-units.
//
// Exact, not rounded: tokens × rate is already in the right scale once the
// answer is a millionth of a unit, because MicroPerUnit and UnitsPer are the
// same number. There is nothing to round, and therefore no rounding bias to
// accumulate and no direction for it to drift in — which is the entire reason
// the ledger carries micro-units rather than whole units.
//
// A zero or negative token count is free rather than a credit, so an engine
// that reports nonsense cannot mint money.
func units(tokens int, rate int64) Amount {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	return Amount(int64(tokens) * rate)
}

// Estimate is a worst-case price for a request that has not run yet.
//
// It is what the reservation path uses, so it must be an upper bound: reserving
// less than the request can cost is the same as not reserving at all. The prompt
// count passed in is the gateway's own estimate, and maxTokens is the client's
// ceiling.
//
// A model with no price estimates to zero, and the caller decides what that
// means — silently reserving nothing for an unpriced model would let an unbilled
// request consume a GPU.
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

// AnyPriced reports whether the book holds a price for anything.
func (p *Pricer) AnyPriced() bool { return len(p.book.prices) > 0 }

// EstimateAtCheapest prices a token count at the lowest input rate in force.
//
// It exists for the budget's reservation fallback and nothing else. Charging
// this model is always wrong, so the name says what it is: an estimate, not a
// charge. Zero means nothing is priced at all, which a caller has to be able to
// tell apart from "priced at zero" — hence AnyPriced alongside it.
//
// Every token goes at the input rate, matching Estimate. Charging a completion
// as input would over-reserve by up to the output/input ratio, which on a
// cheap-input/expensive-output model is a large multiple of the tenant's real
// per-request cost and would make a modest budget unusable.
func (p *Pricer) EstimateAtCheapest(tokens int) Amount {
	if len(p.book.prices) == 0 {
		return 0
	}
	cheapest := p.book.prices[p.book.Models()[0]]
	for _, m := range p.book.Models() {
		if r := p.book.prices[m]; r.Input < cheapest.Input {
			cheapest = r
		}
	}
	return units(tokens, cheapest.Input)
}
