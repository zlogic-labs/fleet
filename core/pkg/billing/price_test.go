package billing

import (
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

func book(t *testing.T, prices ...Price) *Book {
	t.Helper()
	b, err := NewBook(prices)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	return b
}

func standard(t *testing.T) *Pricer {
	t.Helper()
	return NewPricer(book(t, Price{
		Model: "demo",
		Rate:  Rate{Input: 1000, Output: 2000, Cached: 100},
	}))
}

// The wire format's prompt_tokens INCLUDES cached_tokens. Charging the whole
// prompt at the input rate and then charging the cached portion again at the
// cached rate bills every cache hit twice — the single most expensive mistake
// available in this package, and one that looks like a reasonable invoice.
func TestCachedTokensAreNotChargedTwice(t *testing.T) {
	p := standard(t)
	// 1000 prompt, 400 of which were cached; 500 generated.
	u := openai.Usage{
		PromptTokens:        1000,
		CompletionTokens:    500,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 400},
	}
	got, err := p.Charge("demo", u)
	if err != nil {
		t.Fatal(err)
	}
	// 600 fresh @1000 + 400 cached @100 + 500 output @2000, per 1M.
	want := Amount(600*1000 + 400*100 + 500*2000)
	if got != want {
		t.Errorf("charge = %d, want %d", got, want)
	}
	// And explicitly: the wrong way round would be materially larger.
	naive := Amount(1000*1000 + 400*100 + 500*2000)
	if got == naive {
		t.Error("the cached portion was charged at the fresh input rate as well")
	}
}

// completion_tokens INCLUDES reasoning_tokens, for the same reason.
func TestReasoningTokensAreNotChargedTwice(t *testing.T) {
	p := standard(t)
	u := openai.Usage{
		PromptTokens:            100,
		CompletionTokens:        800,
		TotalTokens:             900,
		CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: 600},
	}
	got, err := p.Charge("demo", u)
	if err != nil {
		t.Fatal(err)
	}
	// 100 fresh @1000 + 200 plain output @2000 + 600 reasoning @2000 (no override).
	want := Amount(100*1000 + 200*2000 + 600*2000)
	if got != want {
		t.Errorf("charge = %d, want %d", got, want)
	}
}

// A reasoning rate that is set is an override, not an extra: the same 600
// reasoning tokens must not be charged twice, at Output and at Reasoning.
func TestReasoningRateOverridesOutputWithoutDoubleCounting(t *testing.T) {
	p := NewPricer(book(t, Price{
		Model: "demo",
		Rate:  Rate{Input: 1000, Output: 2000, Cached: 100, Reasoning: 3000},
	}))
	u := openai.Usage{
		PromptTokens:            100,
		CompletionTokens:        800,
		CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: 600},
	}
	got, err := p.Charge("demo", u)
	if err != nil {
		t.Fatal(err)
	}
	want := Amount(100*1000 + 200*2000 + 600*3000)
	if got != want {
		t.Errorf("charge = %d, want %d", got, want)
	}
}

// A model that reports more cached tokens than prompt tokens is misreporting.
// Subtracting would produce a negative fresh count priced as a large credit.
func TestInconsistentUsageDoesNotProduceACredit(t *testing.T) {
	p := standard(t)
	u := openai.Usage{
		PromptTokens:        100,
		CompletionTokens:    10,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 500},
	}
	got, err := p.Charge("demo", u)
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Errorf("charge = %d, want positive — a negative fresh count billed as a credit", got)
	}

	// 0 fresh (clamped) + 500 cached @100 + 10 completion @2000.
	if want := Amount(500*100 + 10*2000); got != want {
		t.Errorf("charge = %d, want %d", got, want)
	}
}

// An unpriced model must be an error, not a zero. A zero charge is a tenant
// being given a GPU for free, and nothing in the request path would complain.
func TestUnknownModelIsAnErrorNotAZeroCharge(t *testing.T) {
	p := standard(t)
	if _, err := p.Charge("not-priced", openai.Usage{PromptTokens: 1000}); err == nil {
		t.Error("charging an unpriced model succeeded")
	}
	if got := p.Estimate("not-priced", 1000, 1000); got != 0 {
		t.Errorf("Estimate = %d for an unpriced model; the caller must notice", got)
	}
}

// A price book with no prices is a gateway that serves traffic it cannot bill.
func TestEmptyBookIsVisible(t *testing.T) {
	empty, err := NewBook(nil)
	if err != nil {
		t.Fatalf("an empty book should be constructible: %v", err)
	}
	if !empty.Empty() {
		t.Error("a book built from nothing does not report itself empty")
	}
	if standard(t).book.Empty() {
		t.Error("a priced book reports itself empty")
	}
	// A nil book is the "billing not wired up yet" case and must not panic.
	var nilBook *Book
	if !nilBook.Empty() {
		t.Error("a nil book should report empty")
	}
	if _, ok := nilBook.Rate("demo"); ok {
		t.Error("a nil book priced something")
	}
}

// Rounding must not accumulate a bias in the platform's favour. Truncation
// gives up at most one unit per dimension; rounding to nearest would round a
// million small requests in the same direction.
func TestTruncationNeverRoundsUp(t *testing.T) {
	if got := units(1, 1); got != 1 {
		t.Errorf("units(1,1) = %d, want 1 — a tiny request must not be free", got)
	}
	if got := units(999_999, 1); got != 999_999 {
		t.Errorf("units(999999,1) = %d, want 999999", got)
	}
	if got := units(UnitsPer, 1); got != UnitsPer {
		t.Errorf("units(1e6,1) = %d, want %d", got, UnitsPer)
	}
	// Zero and negative quantities are free, not credits.
	if got := units(0, 1000); got != 0 {
		t.Errorf("units(0,1000) = %d", got)
	}
	if got := units(-5, 1000); got != 0 {
		t.Errorf("units(-5,1000) = %d, want 0 — a negative must not become a credit", got)
	}
}

// The estimate is the reservation basis, so it must be an upper bound. The
// prefix cache lives inside the engine, so a hit is unknowable in advance — an
// estimate that assumed one would under-reserve exactly when caching works.
func TestEstimateAssumesNoCacheHit(t *testing.T) {
	p := standard(t)
	// 400 of these prompt tokens would be cached if the engine had them.
	got := p.Estimate("demo", 1000, 500)
	want := Amount(1000*1000 + 500*2000)
	if got != want {
		t.Errorf("Estimate = %d, want %d — the full prompt at the input rate", got, want)
	}
	cached := Amount(600*1000 + 400*100 + 500*2000)
	if cached >= want {
		t.Error("an estimate that assumed a cache hit was not an upper bound; it cannot be, and it must not pretend to be")
	}
}

// The book must reject prices that cannot be used, rather than billing something
// plausible.
func TestInvalidPricesAreRejectedAtLoad(t *testing.T) {
	cases := []struct {
		name string
		p    Price
		want string
	}{
		{"no model", Price{Rate: Rate{Input: 1, Output: 1}}, "model is required"},
		{"zero input", Price{Model: "m"}, "input rate must be positive"},
		{"zero output", Price{Model: "m", Rate: Rate{Input: 1}}, "output rate must be positive"},
		{"negative cached", Price{Model: "m", Rate: Rate{Input: 1, Output: 1, Cached: -1}}, "must not be negative"},
		{"cached above input", Price{Model: "m", Rate: Rate{Input: 100, Output: 1, Cached: 200}}, "exceeds input rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBook([]Price{tc.p})
			if err == nil {
				t.Fatal("NewBook accepted an unusable price")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Two prices for one model means the book silently picked one, and which one
// depended on iteration order.
func TestDuplicateModelIsRejected(t *testing.T) {
	_, err := NewBook([]Price{
		{Model: "m", Rate: Rate{Input: 1, Output: 1}},
		{Model: "m", Rate: Rate{Input: 2, Output: 2}},
	})
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("err = %v, want a duplicate-model error", err)
	}
}

// Zero cached rate is allowed — it means "this model has no cache", which a
// llama.cpp deployment genuinely is.
func TestZeroCachedRateIsAllowed(t *testing.T) {
	if _, err := NewBook([]Price{{Model: "m", Rate: Rate{Input: 1, Output: 1}}}); err != nil {
		t.Errorf("a model without a cache rate was rejected: %v", err)
	}
	p := NewPricer(book(t, Price{Model: "m", Rate: Rate{Input: 1000, Output: 1000}}))
	u := openai.Usage{
		PromptTokens:        100,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 50},
	}
	// 50 fresh @1000 + 50 cached @0.
	if got, _ := p.Charge("m", u); got != Amount(50*1000) {
		t.Errorf("charge = %d, want 50", got)
	}
}
