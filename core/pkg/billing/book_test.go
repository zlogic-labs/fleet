package billing

import (
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

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
	if _, ok := nilBook.Rate("demo", ""); ok {
		t.Error("a nil book priced something")
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
	if got, _ := p.Charge("m", "", u); got != Amount(50*1000) {
		t.Errorf("charge = %d, want 50", got)
	}
}
