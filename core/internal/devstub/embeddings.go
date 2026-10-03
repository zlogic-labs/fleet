package devstub

import (
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The embeddings endpoint of the demo engine.
//
// It exists so the gateway's /v1/embeddings path can be exercised on a laptop
// with no GPU and no weights. The vectors are deterministic functions of the
// input — the same text always gives the same vector — because a stub that
// returned random numbers would make every smoke run a lottery and would hide
// exactly the bug worth finding, which is whether a repeat of the same input
// produces the same output.

// embeddingDims is small on purpose. A real 1536-float vector is 6 KB of
// response per input, and the point here is that the shape, the usage and the
// billing are right, not that the numbers are large.
const embeddingDims = 8

// errInvalid is the error type the stub reports, matching what a real
// OpenAI-compatible engine returns for a malformed request.
const errInvalid = "invalid_request_error"

func (e *Engine) embeddings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model     string `json:"model"`
		Input     any    `json:"input"`
		Dimension *int   `json:"dimensions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errInvalid, "cannot read the request body")
		return
	}

	// Token ids are counted as they arrive, because that is what an engine
	// does: it does not re-tokenize text it was handed as ids. Reporting a
	// usage of zero for an id input would be the stub lying about the one
	// number the gateway bills on.
	switch in := body.Input.(type) {
	case string:
	case []any:
		for _, v := range in {
			switch item := v.(type) {
			case string:
			case []any:
				_ = item
			default:
				writeError(w, http.StatusBadRequest, errInvalid, "input must be a string or an array")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, errInvalid, "input must be a string or an array")
		return
	}

	dims := embeddingDims
	if body.Dimension != nil && *body.Dimension > 0 {
		dims = *body.Dimension
		if dims > 1024 {
			writeError(w, http.StatusBadRequest, errInvalid, "dimensions above 1024 are not served here")
			return
		}
	}

	texts, tokens, err := (&openai.EmbeddingRequest{Input: body.Input}).Inputs()
	if err != nil {
		writeError(w, http.StatusBadRequest, errInvalid, err.Error())
		return
	}

	data := make([]openai.EmbeddingItem, 0, len(texts)+len(tokens))
	total := 0
	for i, s := range texts {
		data = append(data, openai.EmbeddingItem{
			Object: "embedding", Index: i, Embedding: fakeVector(s, dims),
		})
		total += approxTokens(s)
	}
	offset := len(data)
	for i, ids := range tokens {
		data = append(data, openai.EmbeddingItem{
			Object: "embedding", Index: offset + i, Embedding: fakeVector(joinInts(ids), dims),
		})
		total += len(ids)
	}

	// One usage object for the whole batch, which is what OpenAI does and what
	// the gateway's tap reads. A stub that reported usage per item would make
	// the batch arithmetic look wrong for a reason that is not the gateway's.
	resp := openai.EmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  body.Model,
		Usage:  &openai.Usage{PromptTokens: total, TotalTokens: total},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// fakeVector is a stable point on a sphere derived from the text.
//
// The norm is held at 1 so the stub cannot accidentally behave like an engine
// whose embeddings are all near each other, which is a real failure mode and
// worth being visibly absent from.
func fakeVector(s string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	state := h.Sum64()
	out := make([]float32, dims)
	var norm float64
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		v := math.Sin(float64(state>>11)) * 1e-3
		out[i] = float32(v)
		norm += v * v
	}
	if norm == 0 {
		out[0] = 1
		return out
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out
}

// approxTokens counts the same way the gateway's heuristic does for an unknown
// model, so a stub usage and a gateway estimate are the same order of
// magnitude. A stub that reported 1 token per input would make the P6 fallback
// path untestable: it would look like a catastrophic undercount.
func approxTokens(s string) int {
	n := 0
	for _, r := range s {
		if r > 0x2000 {
			n += 2
		} else {
			n++
		}
	}
	return (n + 3) / 4
}

func joinInts(ids []int) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(id))
	}
	return b.String()
}
