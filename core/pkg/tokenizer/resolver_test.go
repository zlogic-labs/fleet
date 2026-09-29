package tokenizer_test

import (
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

func TestResolvePicksLongestMatchingPrefix(t *testing.T) {
	r := tokenizer.NewResolver(0)

	cases := []struct {
		model    string
		encoding string
		kind     tokenizer.Kind
	}{
		{"gpt-4o", "o200k_base", tokenizer.KindExact},
		{"gpt-4o-mini-2024-07-18", "o200k_base", tokenizer.KindExact},
		{"gpt-4-0613", "cl100k_base", tokenizer.KindExact},
		{"gpt-3.5-turbo", "cl100k_base", tokenizer.KindExact},
		// No OpenAI encoding covers these; they must degrade, not fail.
		{"Qwen/Qwen3-32B", "", tokenizer.KindHeuristic},
		{"deepseek-ai/DeepSeek-R1", "", tokenizer.KindHeuristic},
		{"", "", tokenizer.KindHeuristic},
	}

	for _, tc := range cases {
		tok := r.Resolve(tc.model, "")
		if got := tok.Encoding(); got != tc.encoding {
			t.Errorf("Resolve(%q).Encoding() = %q, want %q", tc.model, got, tc.encoding)
		}
		if got := tok.Kind(); got != tc.kind {
			t.Errorf("Resolve(%q).Kind() = %v, want %v", tc.model, got, tc.kind)
		}
	}
}

// The offline loader must satisfy every OpenAI-family request without any
// network access, which is the whole reason Fleet embeds the tables.
func TestExactCountIsPlausibleAndOffline(t *testing.T) {
	tok := tokenizer.NewResolver(0).Resolve("gpt-4o", "")
	if tok.Kind() != tokenizer.KindExact {
		t.Fatal("gpt-4o must resolve to an exact encoding")
	}

	const text = "Kubernetes is a portable, extensible, and scalable platform."
	if n := tok.Count(text); n <= 0 || n > len(text) {
		t.Fatalf("Count(%q) = %d, want a plausible token count", text, n)
	}
}

// An explicit encoding hint must override prefix matching, so that an operator
// can pin a table for a self-hosted model.
func TestEncodingHintOverridesModelName(t *testing.T) {
	tok := tokenizer.NewResolver(0).Resolve("Qwen/Qwen3-32B", "cl100k_base")
	if tok.Encoding() != "cl100k_base" {
		t.Fatalf("Encoding() = %q, want the pinned encoding", tok.Encoding())
	}
	if tok.Count("hello") <= 0 {
		t.Fatal("pinned encoding produced no tokens")
	}
}

// The heuristic biases high on purpose (see heuristic.go) and must never
// report zero for non-empty text, or a request would slip past every
// token-denominated quota.
func TestHeuristicNeverUndercountsToZero(t *testing.T) {
	h := tokenizer.NewHeuristic(0)

	if n := h.Count(""); n != 0 {
		t.Errorf("Count(%q) = %d, want 0", "", n)
	}
	if n := h.Count("x"); n < 1 {
		t.Errorf("Count(%q) = %d, want at least 1", "x", n)
	}

	cjk := strings.Repeat("中", 100)
	if got, want := h.Count(cjk), 100; got < want {
		t.Errorf("Count(100 CJK runes) = %d, want at least %d (1.5/rune)", got, want)
	}
}
