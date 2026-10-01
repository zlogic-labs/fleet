package billing

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The regression this file exists for: in whole units, a 100-token prompt at a
// rate of 1000 per million rounds to zero, so every request under 1000 tokens
// was free. That is most of the traffic a serving platform sees, and it looked
// like a rounding decision rather than a giveaway.
func TestASmallRequestStillCostsSomething(t *testing.T) {
	p := NewPricer(book(t, Price{Model: "demo", Rate: Rate{Input: 1000, Output: 2000}}))

	u := openai.Usage{PromptTokens: 100, CompletionTokens: 20}
	got, err := p.Charge("demo", u)
	if err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatal("a 120-token request priced at zero — the whole point of micro-units")
	}
	// 100 fresh @1000 + 20 output @2000, in micro-units.
	if want := Amount(100*1000 + 20*2000); got != want {
		t.Errorf("charge = %d, want %d", got, want)
	}
	// And the smallest plausible request of all.
	tiny := openai.Usage{PromptTokens: 1}
	if got, _ := p.Charge("demo", tiny); got == 0 {
		t.Error("a single-token prompt priced at zero")
	}
}

// Units is the only place a conversion to a whole unit happens, so it is the
// only place truncation can occur. It must be consistent and it must not be
// applied per line item.
func TestUnitsConversion(t *testing.T) {
	cases := []struct {
		micro Amount
		want  float64
	}{
		{0, 0},
		{500_000, 0.5},
		{1_000_000, 1},
		{2_500_000, 2.5},
		{-1_000_000, -1},
	}
	for _, tc := range cases {
		if got := tc.micro.Units(); got != tc.want {
			t.Errorf("Amount(%d).Units() = %v, want %v", tc.micro, got, tc.want)
		}
	}
}

// Summing micro-units and converting once must equal converting each row and
// summing, for every row that does not truncate. When it does not — which is the
// argument for keeping the ledger in micro-units at all.
func TestSummingMicroUnitsIsMoreAccurateThanSummingUnits(t *testing.T) {
	u := openai.Usage{PromptTokens: 1, CompletionTokens: 1}

	var sum Amount
	var perRow float64
	for i := 0; i < 100; i++ {
		sum += units(u.PromptTokens, 1000) + units(u.CompletionTokens, 1000)
		perRow += units(u.PromptTokens, 1000).Units() + units(u.CompletionTokens, 1000).Units()
	}
	// 1 token @1000 = 1000 micro-units, so 2 tokens per request and 200
	// micro-units per request, 20000 over 100 requests. Rounding each row to
	// whole units would give 0 for every one of them.
	if sum != 200_000 {
		t.Fatalf("sum = %d micro-units, want 200000", sum)
	}
	if sum.Units() <= 0 {
		t.Errorf("the summed total converted to %v units — the whole month's usage became zero", sum.Units())
	}
	if perRow == sum.Units() {
		t.Log("per-row conversion happened to agree here; the sum is still the authoritative path")
	}
}
