// Package tokenizer provides prompt-token counts for admission control.
//
// The authoritative count always comes from the engine's usage field; this
// package exists because the limiter has to decide *before* forwarding, and
// because a limiter that cannot count is a limiter that fails exactly when
// the system is overloaded. See P5 and P6 in docs/architecture.md.
package tokenizer

// Kind describes how much to trust a count. It is carried through to the
// usage record so that a settled ledger entry can be re-derived later.
type Kind int

const (
	// KindExact means an encoding table matched the model.
	KindExact Kind = iota
	// KindHeuristic means the text was estimated by character class.
	KindHeuristic
)

func (k Kind) String() string {
	if k == KindExact {
		return "exact"
	}
	return "heuristic"
}

// Tokenizer counts tokens for one encoding. Implementations must be safe for
// concurrent use; the gateway shares one instance across all requests.
type Tokenizer interface {
	// Count returns the number of tokens the text encodes to.
	Count(text string) int
	// Kind reports whether Count is exact.
	Kind() Kind
	// Encoding names the table used, or "" for the heuristic.
	Encoding() string
}

// Registry resolves a tokenizer for a model identifier. Resolution must never
// fail: an unknown model yields the heuristic rather than an error, because a
// model we cannot count is still a model we have to serve.
type Registry interface {
	For(model string) Tokenizer
}
