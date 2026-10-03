package openai

import "testing"

// Embedding input has four shapes and only one of them is a string. Decoding
// into `any` and reading the strings out is the obvious implementation and it
// is wrong twice over: a token array comes out empty, and an empty input counts
// zero tokens, reserves zero budget and routes on an empty prefix.

func decode(t *testing.T, body string) *EmbeddingRequest {
	t.Helper()
	req, err := DecodeEmbeddingRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return req
}

// words is a stand-in for any tokenizer: one token per whitespace-separated
// word. The point is the arithmetic around it, not the encoding.
func words(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
		}
	}
	if len(s) > 0 {
		n++
	}
	return n
}

func TestASingleStringIsOneInput(t *testing.T) {
	req := decode(t, `{"model":"m","input":"hello"}`)
	texts, tokens, err := req.Inputs()
	if err != nil {
		t.Fatalf("inputs: %v", err)
	}
	if len(texts) != 1 || texts[0] != "hello" || len(tokens) != 0 {
		t.Fatalf("got texts=%v tokens=%v", texts, tokens)
	}
	if got := req.Prefix(100); got != "hello" {
		t.Errorf("prefix = %q, want %q", got, "hello")
	}
}

func TestAnArrayOfStringsIsABatch(t *testing.T) {
	req := decode(t, `{"model":"m","input":["one two","three"]}`)
	n, err := req.PromptTokens(words)
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	if n != 3 {
		t.Errorf("prompt tokens = %d, want 3 (two plus one)", n)
	}
	// The prefix is the head of the first input, not the whole batch: the
	// routing key has to be stable for a document that will be asked for again.
	if got := req.Prefix(100); got != "one two" {
		t.Errorf("prefix = %q, want the first input", got)
	}
}

func TestTokenIdsAreCountedWithoutATokenizer(t *testing.T) {
	req := decode(t, `{"model":"m","input":[[1,2,3],[4,5]]}`)
	texts, tokens, err := req.Inputs()
	if err != nil {
		t.Fatalf("inputs: %v", err)
	}
	if len(texts) != 0 {
		t.Fatalf("token input produced texts %v", texts)
	}
	if len(tokens) != 2 || len(tokens[0]) != 3 || len(tokens[1]) != 2 {
		t.Fatalf("tokens = %v", tokens)
	}
	// Counting must not consult the tokenizer: there is no text to tokenize,
	// and a count function that ran anyway would return 0 and reserve nothing.
	called := false
	n, err := req.PromptTokens(func(string) int { called = true; return 99 })
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	if called {
		t.Error("the tokenizer was consulted for token ids, which have no text")
	}
	if n != 5 {
		t.Errorf("prompt tokens = %d, want 5", n)
	}
}

func TestARequestWithNoModelIsRefused(t *testing.T) {
	if _, err := DecodeEmbeddingRequest([]byte(`{"input":"hi"}`)); err == nil {
		t.Fatal("a request with no model was accepted")
	}
}

func TestUnusableInputIsRefusedRatherThanCountedAsZero(t *testing.T) {
	// Every one of these would count zero tokens if it were accepted, and a
	// zero-token request reserves nothing — so accepting it would hand a caller
	// an unmetered path through every limit on the platform.
	for _, body := range []string{
		`{"model":"m"}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","input":123}`,
		`{"model":"m","input":[[]]}`,
		`{"model":"m","input":[[1,"two"]]}`,
		`{"model":"m","input":["a",[1,2]]}`,
	} {
		req := decode(t, body)
		if _, err := req.PromptTokens(words); err == nil {
			t.Errorf("%s was accepted and counted", body)
		}
	}
}

func TestThePrefixIsTruncatedToRunesNotBytes(t *testing.T) {
	req := decode(t, `{"model":"m","input":["héllo wörld"]}`)
	// Five runes fits; four must not split a multi-byte character in half,
	// which would produce a string that is not valid UTF-8 and a hash that
	// does not correspond to any prefix of the input.
	if got := req.Prefix(5); got != "héllo" {
		t.Errorf("prefix(5) = %q, want %q", got, "héllo")
	}
}
