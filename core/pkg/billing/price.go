// Package billing prices usage and records it in the ledger.
//
// The arithmetic lives in pricer.go; this file holds what a price is.
//
// Two rules decide everything here.
//
// First, P8: cost is an allocation of a fixed pool, not a resale price. The
// pool for a month is known only after the month ends, so no code path may
// treat a currency amount as the authoritative figure at request time. What a
// price book stores is therefore a *rate in quota units*, and money is derived
// later. That is what makes pre-enforcement possible at all: a budget can be
// checked against units because the units are known, while the yuan figure is
// not.
//
// Second, P6: the engine's usage is the only authority. Every quantity below
// comes from the engine's response, never from the client.
package billing

import "fmt"

// Units are what a price is expressed in: 1M tokens of a given model.
//
// A 1M-token base rather than a per-token rate keeps the price book readable
// (the numbers look like the ones vendors publish) and keeps the arithmetic in
// integers. A float would make "why did my bill move by a millionth" a question
// with no good answer.
const UnitsPer = 1_000_000

// Rate is a price per UnitsPer tokens, in quota units.
//
// The three rates are the ones that differ across models and are the ones that
// decide the bill. Anything not modelled here is billed as the rate that
// contains it, which is stated explicitly rather than left to chance — see
// Pricer.charge.
type Rate struct {
	// Input is a fresh prompt token. Prompt tokens that hit the prefix cache
	// are Cached instead.
	Input int64 `json:"input"`
	// Output is a generated token.
	Output int64 `json:"output"`
	// Cached is a prompt token the engine served from its prefix cache.
	//
	// Kept low deliberately: the platform's margin comes from prefix-cache hit
	// rate (§6), so charging a cache hit at anything near the fresh-input rate
	// taxes exactly the behaviour the platform is trying to produce.
	Cached int64 `json:"cached"`
	// Reasoning is a thinking token. Charged at Output, so this is an override
	// rather than a fourth dimension: a model that does not expose reasoning
	// tokens leaves it zero and the tokens land on Output anyway.
	Reasoning int64 `json:"reasoning,omitempty"`
}

// zero reports whether a rate carries no price at all.
func (r Rate) zero() bool {
	return r.Input == 0 && r.Output == 0 && r.Cached == 0 && r.Reasoning == 0
}

// Price is one model's rates.
//
// It is a value with no methods beyond validation because a price book entry
// has no behaviour; anything that looks like behaviour here is arithmetic that
// Pricer owns, and arithmetic that both the gateway and the console might need
// to redo is arithmetic that will eventually disagree between them.
type Price struct {
	// Model is the *resolved* model — the one the endpoint serves, not the
	// string the client typed. An alias pointing at an expensive model must be
	// billed at the expensive model's price, or aliases become a way to buy the
	// expensive model cheaply.
	Model string `json:"model"`
	Rate  `json:",inline"`

	// Currency is informational. It is not what the ledger balances in; see the
	// package comment.
	Currency string `json:"currency,omitempty"`
}

// Validate rejects a price that cannot be used.
//
// A zero input rate is rejected rather than defaulted: a model with no input
// price is a typo in a config file, and silently substituting the output rate
// would produce a bill that looks plausible and is wrong.
func (p Price) Validate() error {
	if p.Model == "" {
		return fmt.Errorf("price: model is required")
	}
	if p.Rate.Input <= 0 {
		return fmt.Errorf("price %s: input rate must be positive, got %d", p.Model, p.Input)
	}
	if p.Rate.Output <= 0 {
		return fmt.Errorf("price %s: output rate must be positive, got %d", p.Model, p.Output)
	}
	if p.Rate.Cached < 0 {
		return fmt.Errorf("price %s: cached rate must not be negative", p.Model)
	}
	// A cache rate above the fresh input rate would charge a cache hit more than
	// a miss, which no provider does and no operator intends. Catching it here
	// turns a pricing mistake into a startup error instead of a bill.
	if p.Rate.Cached > 0 && p.Rate.Cached > p.Rate.Input {
		return fmt.Errorf("price %s: cached rate %d exceeds input rate %d",
			p.Model, p.Rate.Cached, p.Input)
	}
	return nil
}

// Book is a set of prices, looked up by model.
//
// A plain map rather than a database or a cached loader: the gateway resolves a
// price per request, and an interface on that path is a nil check and an error
// branch that both have to be right. The book is loaded once at startup from
// configuration or the control plane, and replacing it is a pointer swap.
type Book struct {
	prices map[string]Rate
}

// NewBook indexes prices by model and rejects an invalid one.
//
// It takes a slice rather than a map so a duplicate model is reported. A map
// literal would silently keep the last entry, and which of two prices won would
// depend on iteration order.
func NewBook(prices []Price) (*Book, error) {
	b := &Book{prices: make(map[string]Rate, len(prices))}
	for _, p := range prices {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := b.prices[p.Model]; dup {
			return nil, fmt.Errorf("price %s: declared twice", p.Model)
		}
		b.prices[p.Model] = p.Rate
	}
	return b, nil
}

// Rate returns the rates for a model.
//
// The second return is false for an unknown model rather than a zero Rate. A
// zero rate would price every token at nothing, and a tenant with an unbilled
// model is a tenant nobody notices is being given a GPU for free.
func (b *Book) Rate(model string) (Rate, bool) {
	if b == nil {
		return Rate{}, false
	}
	r, ok := b.prices[model]
	return r, ok
}

// Models lists the priced models, for the console.
func (b *Book) Models() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.prices))
	for m := range b.prices {
		out = append(out, m)
	}
	return out
}

// Empty reports whether the book prices nothing.
//
// A gateway that starts with an empty book is a gateway that can serve traffic
// it cannot bill, which is the one state the billing layer exists to prevent.
func (b *Book) Empty() bool { return b == nil || len(b.prices) == 0 }
