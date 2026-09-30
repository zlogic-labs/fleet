package engine

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

func TestFormatOfDistinguishesLayoutsFromFilesAlone(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  weights.Format
	}{
		{"sharded", []string{"config.json", "model-00001-of-00002.safetensors"}, weights.Safetensors},
		{"single file", []string{"config.json", "model.safetensors"}, weights.Safetensors},
		{"gguf has no config", []string{"model-Q4_K_M.gguf", "README.md"}, weights.GGUF},
		{"gguf wins over a stray config", []string{"config.json", "model.gguf"}, weights.GGUF},
		{"neither", []string{"README.md", "LICENSE"}, weights.Unknown},
		{"nil", nil, weights.Unknown},
	}
	for _, c := range cases {
		if got := weights.Of(c.files); got != c.want {
			t.Errorf("%s: format = %q, want %q", c.name, got, c.want)
		}
	}
}

// An engine nobody described must not inherit another's rules. Falling back to
// the vLLM profile would let a GGUF model be declared loadable by an engine
// that cannot read it, which is the exact failure the fallback exists to avoid.
func TestUnknownEngineDoesNotBorrowAnotherProfilesRules(t *testing.T) {
	got := BuiltinProfiles().For("some-engine-nobody-has-heard-of")
	if got.Format != weights.Unknown {
		t.Errorf("format = %q, want unknown", got.Format)
	}
	if got.MinCompute != 0 {
		t.Errorf("MinCompute = %d, want 0: a fallback must not refuse cards", got.MinCompute)
	}
	if got.Metrics.Available() {
		t.Error("an undeclared engine must not claim autoscaling signals")
	}
}

func TestForMatchesAnEngineFamilyPrefix(t *testing.T) {
	// A deployment may name a versioned or suffixed engine. Losing the weight
	// format on a spelling difference would silently permit GGUF to vLLM.
	for _, name := range []string{"vllm", "VLLM", "vllm-0.9.1", "vllm-ha"} {
		if got := BuiltinProfiles().For(name); got.Format != weights.Safetensors {
			t.Errorf("%s: format = %q, want safetensors", name, got.Format)
		}
	}
}

// Every group needs one match, not all of them. A safetensors repository has a
// single model.safetensors or an index plus shards, never both, so requiring
// both would report a correct repository as incomplete.
func TestVerifyFilesTreatsGroupsAsAlternatives(t *testing.T) {
	prof := VLLMProfile()
	cases := []struct {
		name  string
		files []string
		ok    bool
	}{
		{"unsharded", []string{"config.json", "model.safetensors"}, true},
		{"sharded", []string{"config.json", "model.safetensors.index.json", "model-00001-of-00002.safetensors"}, true},
		{"missing config", []string{"model.safetensors"}, false},
		{"index but no shards", []string{"config.json", "model.safetensors.index.json"}, false},
		{"gguf does not satisfy vllm", []string{"config.json", "model.gguf"}, false},
	}
	for _, c := range cases {
		if missing := prof.VerifyFiles(c.files); (len(missing) == 0) != c.ok {
			t.Errorf("%s: missing = %v, want ok=%v", c.name, missing, c.ok)
		}
	}
}

func TestCompatibleIsRefusedBothWays(t *testing.T) {
	gguf := weights.Of([]string{"model.gguf"})
	if got := Compatible(gguf, "llama-cpp"); got != nil {
		t.Errorf("llama-cpp should load gguf, got %v", got)
	}
	if err := Compatible(gguf, "vllm"); err == nil {
		t.Error("vllm must refuse gguf rather than discovering it at load time")
	}
	if err := Compatible(weights.Unknown, "vllm"); err == nil {
		t.Error("a model of unknown format must be refused, not guessed at")
	}
}
