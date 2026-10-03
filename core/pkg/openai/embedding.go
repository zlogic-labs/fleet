package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Embedding input is four shapes, and the difference between two of them is
// the difference between counting tokens and not counting them.
//
// OpenAI accepts a string, an array of strings, an array of token ids, and an
// array of arrays of token ids. Decoding into `any` and hoping is not an
// option here: a token array read as a string list comes out empty, and an
// empty input counts zero tokens, reserves zero budget and routes on an empty
// prefix. Every one of those is a free request on somebody else's GPU.

// Texts returns the textual inputs and the token inputs separately.
//
// Exactly one of the two is populated. Token arrays are returned as-is because
// there is no text to count or to hash: their length is the token count, and
// the routing key has to be built from the ids rather than from a string that
// does not exist.
func (r *EmbeddingRequest) Inputs() (texts []string, tokens [][]int, err error) {
	switch in := r.Input.(type) {
	case nil:
		return nil, nil, fmt.Errorf("input is required")
	case string:
		return []string{in}, nil, nil
	case []any:
		if len(in) == 0 {
			return nil, nil, fmt.Errorf("input is an empty array")
		}
		for i, v := range in {
			switch item := v.(type) {
			case string:
				texts = append(texts, item)
			case []any:
				ids := make([]int, 0, len(item))
				for _, t := range item {
					f, ok := t.(float64)
					if !ok || f < 0 || f != float64(int64(f)) {
						return nil, nil, fmt.Errorf("input[%d] holds a token that is not a whole number", i)
					}
					ids = append(ids, int(f))
				}
				if len(ids) == 0 {
					return nil, nil, fmt.Errorf("input[%d] is an empty token array", i)
				}
				tokens = append(tokens, ids)
			default:
				return nil, nil, fmt.Errorf(
					"input[%d] is neither a string nor an array of token ids", i)
			}
		}
		if len(texts) > 0 && len(tokens) > 0 {
			// Mixing the two is legal on the wire and ambiguous here: a batch
			// would be half countable and half not, and the reservation would
			// cover only the part that could be measured.
			return nil, nil, fmt.Errorf("input mixes strings and token arrays; send one or the other")
		}
		return texts, tokens, nil
	default:
		return nil, nil, fmt.Errorf("input must be a string or an array")
	}
}

// PromptTokens estimates what the request will be charged before it runs.
//
// An embedding produces no completion, so this is the whole cost. Reserving a
// default completion budget on top of it would let one enormous input pass a
// limit that a thousand small ones cannot, which is the opposite of what the
// reservation is for.
func (r *EmbeddingRequest) PromptTokens(count func(string) int) (int, error) {
	texts, tokens, err := r.Inputs()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, t := range tokens {
		total += len(t)
	}
	for _, s := range texts {
		total += count(s)
	}
	return total, nil
}

// Prefix is the routing-affinity key: the head of the first input, truncated.
//
// Embeddings are order-independent and their prefix cache is keyed per input,
// so there is no conversation head to take. The head of the first input is the
// closest stable key that exists, and it is what a repeated batch of documents
// will actually share.
func (r *EmbeddingRequest) Prefix(runeLen int) string {
	texts, _, err := r.Inputs()
	if err != nil || len(texts) == 0 {
		return ""
	}
	s := texts[0]
	if runeLen <= 0 {
		return s
	}
	if len([]rune(s)) <= runeLen {
		return s
	}
	return string([]rune(s)[:runeLen])
}

// DecodeEmbeddingRequest is the single entry point for reading an embeddings
// body, so a malformed body produces the same error shape as a malformed chat
// body.
func DecodeEmbeddingRequest(body []byte) (*EmbeddingRequest, error) {
	var req EmbeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	return &req, nil
}

// EmbeddingInputTokens counts token-array inputs without a tokenizer, which is
// what makes the token form of `input` usable at all on a gateway that does not
// know the model's encoding.
func EmbeddingInputTokens(body []byte) int {
	var req struct {
		Input any `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}
	total := 0
	items, ok := req.Input.([]any)
	if !ok {
		return 0
	}
	for _, it := range items {
		if ids, ok := it.([]any); ok {
			total += len(ids)
		}
	}
	return total
}
