package billing

import (
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

func standardPrice() Price {
	return Price{
		Model: "demo",
		Rate:  Rate{Input: 1000, Output: 2000, Cached: 100},
	}
}

func standard(t *testing.T) *Pricer {
	t.Helper()
	return NewPricer(book(t, standardPrice()))
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
	got, err := p.Charge("demo", "", u)
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
	got, err := p.Charge("demo", "", u)
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
	got, err := p.Charge("demo", "", u)
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
	got, err := p.Charge("demo", "", u)
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

// An unpriced model must be an error to charge against, and a reservation that
// says it does not know.
//
// A zero charge is a tenant being given a GPU for free, and nothing in the
// request path would complain. The reservation cannot be zero either, so Predict
// falls back to the cheapest rate in the same centre and reports Known=false;
// the caller is expected to act on that rather than trust the number.
func TestUnknownModelIsAnErrorNotAZeroCharge(t *testing.T) {
	p := standard(t)
	if _, err := p.Charge("not-priced", "", openai.Usage{PromptTokens: 1000}); err == nil {
		t.Error("charging an unpriced model succeeded")
	}
	got := p.Predict("not-priced", "", 1000, 1000)
	if got.Known {
		t.Error("Predict claimed to know the price of an unpriced model")
	}
	if got.Amount == 0 {
		t.Error("Predict reserved nothing for an unpriced model; the budget it guards would not bind")
	}
}

// An unpriced model must not borrow another cost centre's rate.
//
// A pool weight and a vendor price are three orders of magnitude apart, so a
// vendor request reserved against a fleet weight would under-reserve by that
// factor and let a tenant spend a month of budget in an afternoon.
func TestAnUnpricedModelDoesNotBorrowAnotherCentresRate(t *testing.T) {
	vendor := Price{Model: "gpt-4o", Provider: "openai",
		Rate: Rate{Input: 2_000_000, Output: 6_000_000}}
	book, err := NewBook([]Price{vendor, standardPrice()})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPricer(book)

	vendorOnly := p.Predict("gpt-4o", "openai", 1000, 500)
	if !vendorOnly.Known {
		t.Fatal("a priced vendor model must be known")
	}
	fleetFallback := p.Predict("unpriced-anywhere", "", 1000, 500)
	if fleetFallback.Known {
		t.Fatal("premise: an unpriced model must not be reported as known")
	}
	// The fallback is the cheapest fleet rate, not the vendor's. The vendor rate
	// is the largest number in the book, so borrowing it would show up as an
	// over-reservation by three orders of magnitude.
	if fleetFallback.Amount >= vendorOnly.Amount {
		t.Errorf("the fleet fallback (%d) is not below the vendor price (%d); it looks like it borrowed one",
			fleetFallback.Amount, vendorOnly.Amount)
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

// The reservation is the upper bound on the request. The prefix cache lives
// inside the engine, so a hit is unknowable in advance — a prediction that
// assumed one would under-reserve exactly when caching works.
func TestPredictionAssumesNoCacheHit(t *testing.T) {
	p := standard(t)
	// 400 of these prompt tokens would be cached if the engine had them.
	got := p.Predict("demo", "", 1000, 500)
	want := Amount(1000*1000 + 500*2000)
	if got.Amount != want {
		t.Errorf("Predict = %d, want %d — the full prompt at the input rate", got.Amount, want)
	}
	cached := Amount(600*1000 + 400*100 + 500*2000)
	if cached >= want {
		t.Error("a prediction that assumed a cache hit was not an upper bound; it cannot be, and it must not pretend to be")
	}
}
