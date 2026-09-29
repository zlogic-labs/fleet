package tokenizer

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// Resolver picks a tokenizer for a request. The encodingHint comes from the
// model's TokenizerID in the FleetModel CRD and wins over prefix matching, so
// an operator can pin a table for a self-hosted model whose name follows no
// OpenAI convention.
type Resolver interface {
	Resolve(model, encodingHint string) Tokenizer
}

// modelEncodings lists the model-name prefixes served by each OpenAI encoding.
// Self-hosted models (Qwen, Llama, DeepSeek, ...) match nothing here and fall
// through to the heuristic by design — see NewResolver.
var modelEncodings = map[string][]string{
	"o200k_base":  {"gpt-4o", "gpt-4.1", "gpt-4.5", "o1", "o3", "o4", "chatgpt-4o"},
	"cl100k_base": {"gpt-4", "gpt-3.5", "text-embedding-3", "text-embedding-ada"},
	"p50k_base":   {"code-", "text-davinci"},
	"r50k_base":   {"gpt-3"},
	"gpt2":        {"gpt2"},
}

type resolver struct {
	fallback Tokenizer

	mu    sync.RWMutex
	cache map[string]Tokenizer
}

// fallbackOnError is the tokenizer used when a BPE table is unavailable.
// Allocated once because Count sits on the hot path of every request.
var fallbackOnError = NewHeuristic(1)

// useOfflineTablesOnce guards the process-wide BPE loader swap. The loader is
// a global in tiktoken-go, so it is configured here rather than in init():
// mutating another library's state as an import side effect would make the
// behaviour unobservable to anything else in the binary.
var useOfflineTablesOnce sync.Once

// NewResolver builds the default registry. bias scales heuristic counts; pass
// 0 for the calibrated 1.0.
//
// BPE tables are loaded from the embedded loader rather than downloaded, so an
// air-gapped install resolves OpenAI-family models correctly. Tables for
// self-hosted models are not bundled: those use the heuristic until an
// operator pins an encoding, and the engine's own usage field remains
// authoritative for billing (P6).
func NewResolver(bias float64) Resolver {
	useOfflineTablesOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	})
	if bias <= 0 {
		bias = 1
	}
	return &resolver{
		fallback: NewHeuristic(bias),
		cache:    make(map[string]Tokenizer),
	}
}

func (r *resolver) Resolve(model, encodingHint string) Tokenizer {
	if name := resolveEncoding(model, encodingHint); name != "" {
		if t := r.load(name); t != nil {
			return t
		}
	}
	return r.fallback
}

// resolveEncoding returns an encoding name or "" when nothing matches.
// The longest matching prefix wins, so "gpt-4o" beats a hypothetical "gpt-4"
// rule without the table needing to be ordered.
func resolveEncoding(model, hint string) string {
	if hint != "" {
		return hint
	}
	lower := strings.ToLower(model)
	best, bestLen := "", 0
	for name, prefixes := range modelEncodings {
		for _, p := range prefixes {
			if len(p) > bestLen && strings.HasPrefix(lower, p) {
				best, bestLen = name, len(p)
			}
		}
	}
	return best
}

// load returns a cached tokenizer for the encoding, or nil if the table is
// unavailable. A failure here degrades that model to the heuristic rather than
// failing the request.
func (r *resolver) load(name string) Tokenizer {
	r.mu.RLock()
	t, ok := r.cache[name]
	r.mu.RUnlock()
	if ok {
		return t
	}

	core, err := tiktoken.GetEncoding(name)
	if err != nil {
		return nil
	}
	t = bpeTokenizer{core: core, name: name}

	r.mu.Lock()
	r.cache[name] = t
	r.mu.Unlock()
	return t
}

type bpeTokenizer struct {
	core *tiktoken.Tiktoken
	name string
}

// Count uses EncodeOrdinary, which never fails: it refuses to interpret the
// reserved special-token strings as single tokens, which is the correct
// behaviour for a request body that has already been serialised.
func (b bpeTokenizer) Count(text string) int {
	if text == "" {
		return 0
	}
	return len(b.core.EncodeOrdinary(text))
}

func (b bpeTokenizer) Kind() Kind       { return KindExact }
func (b bpeTokenizer) Encoding() string { return b.name }
