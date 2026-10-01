package engine

import (
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// DefaultProfile is the profile for an engine nobody has described.
//
// It assumes the OpenAI surface and nothing else: no extensions are probed, no
// metrics are claimed, and no weight format is asserted. Assuming less is the
// point — a wrong guess about an unknown engine is a wrong readiness verdict,
// and readiness is what decides whether traffic arrives.
func DefaultProfile() Profile {
	return Profile{
		Name:          "openai-compatible",
		Format:        weights.Unknown,
		RequiredFiles: [][]string{{"config.json"}},
		Health:        Candidates{"/health"},
		Version:       Candidates{"/version"},
		Metrics:       MetricsSpec{},
		Notes:         "Unrecognised engine: only the OpenAI-compatible surface is assumed.",
	}
}

// vLLMProfile describes vLLM.
//
// The three numbers here are the ones that decide whether a node is a
// candidate at all, and they are the numbers people get wrong. vLLM's own
// installation documentation states compute capability 7.5 or higher, so a
// Pascal or Volta card is refused up front instead of producing a Pod that
// loads weights and then fails on the first kernel launch.
func VLLMProfile() Profile {
	return Profile{
		Name:   "vllm",
		Format: weights.Safetensors,
		RequiredFiles: [][]string{
			{"config.json"},
			// A repository is either one unsharded file or an index pointing at
			// several shards. Demanding both, or the index specifically, marks
			// a correctly-shaped repository as incomplete.
			{"model.safetensors", "model.safetensors.index.json"},
			{"*.safetensors"},
		},
		Health:  Candidates{"/health"},
		Version: Candidates{"/version"},
		Tokenize: Candidates{
			// /tokenize is a vLLM extension outside the OpenAI spec. It is the
			// cheap route to exact prompt counts, and its absence is why the
			// gateway keeps a local tokenizer and an estimator.
			"/tokenize",
			// Newer builds moved it under the API prefix.
			"/v1/tokenize",
		},
		Metrics: MetricsSpec{
			Path: "/metrics",
			Series: map[Signal]string{
				SignalQueueDepth:  "vllm:num_requests_waiting",
				SignalRunningReqs: "vllm:num_requests_running",
				// vllm:kv_cache_usage_perc, not vllm:gpu_cache_usage_perc. The
				// latter name was never a real metric in any vLLM release — it
				// was an assumption about which component owns the cache —
				// and a wrong series name reads as zero usage, which is a
				// perfectly healthy-looking deployment. Verified against
				// vllm/v1/metrics/loggers.py on main at 2eaa3bc, 2026-10-01.
				SignalKVCacheUsed: "vllm:kv_cache_usage_perc",
			},
			// Capacity is reported as labels on an info gauge whose value is
			// always 1. Field names verified in vllm/config/cache.py on main
			// at 2eaa3bc: kv_cache_size_tokens is "Per-DP-engine KV cache
			// capacity in tokens (group-aware)" and kv_cache_max_concurrency
			// is "Per-DP-engine maximum concurrency at max_model_len tokens".
			InfoGauge:           "vllm:cache_config_info",
			KVTokensLabel:       "kv_cache_size_tokens",
			MaxConcurrencyLabel: "kv_cache_max_concurrency",
		},
		MinCompute: 75,
		Notes:      "Requires compute capability 7.5+ (T4, RTX 20-series and newer). No MIG before 7.5.",
	}
}

// LlamaCPPProfile describes llama.cpp's llama-server.
//
// The point of this profile is that it is honest about what llama.cpp does not
// have. There is no /tokenize, so the gateway's local tokenizer is the only
// exact path for these deployments; there is no vLLM-style metric set, so
// autoscaling has nothing to scale on; and it runs on cards vLLM refuses
// outright, which is exactly why an operator reaches for it.
func LlamaCPPProfile() Profile {
	return Profile{
		Name:   "llama-cpp",
		Format: weights.GGUF,
		RequiredFiles: [][]string{
			// A GGUF repository is one or a few .gguf files and no config.json.
			{"*.gguf"},
		},
		Health: Candidates{
			"/health",
			// Older and forked servers used this.
			"/api/health",
		},
		Version: Candidates{
			// llama-server exposes its build under /props rather than
			// /version, but both are tried and neither is required.
			"/props",
			"/version",
		},
		// Deliberately empty. llama-server serves no /tokenize, so probing for
		// it costs a round trip on every reconcile to learn what the profile
		// already knows. An engine that adds one later gets a new profile.
		Tokenize: nil,
		Metrics: MetricsSpec{
			// llama-server does publish Prometheus metrics, under its own
			// llamacpp: prefix and with its own names. Two things about it are
			// worth stating rather than discovering at 3am: the endpoint is
			// disabled unless the server is started with --metrics, answering
			// 501 otherwise, and there is no KV cache capacity among them, so
			// there is nothing to declare an InfoGauge for. Names verified
			// against tools/server/README.md on master, 2026-10-01.
			Path: "/metrics",
			Series: map[Signal]string{
				// requests_deferred counts requests that are queued rather
				// than running, which is the same signal vLLM splits across
				// num_requests_waiting and num_requests_waiting_by_reason.
				SignalQueueDepth:  "llamacpp:requests_deferred",
				SignalRunningReqs: "llamacpp:requests_processing",
				// No cache-usage equivalent is published. SignalKVCacheUsed is
				// deliberately left unmapped rather than pointed at
				// llamacpp:n_tokens_max, which is an observed high-water mark
				// of context size and not an occupancy — reading it as usage
				// would make a nearly empty server look full.
			},
			InfoGauge:           "",
			KVTokensLabel:       "",
			MaxConcurrencyLabel: "",
		},
		MinCompute: 0,
		Notes: "GGUF only. No /tokenize, so prompt counts come from the gateway's own " +
			"tokenizer. Metrics require the server to be started with --metrics, and it " +
			"publishes no KV cache capacity.",
	}
}

// BuiltinProfiles returns the profiles Fleet ships.
//
// Only vLLM and llama.cpp for now. A third is one more literal, which is the
// entire reason this is data.
func BuiltinProfiles() *Profiles {
	return NewProfiles(VLLMProfile(), LlamaCPPProfile())
}

// Compatible reports whether an engine family can load a given weight format,
// and why not when it cannot.
//
// It is the one place that answers that question, and both callers need the
// reason: the puller tells an operator which engine to target, and the console
// greys out the ones that would fail. A deployment that discovers the mismatch
// as a crash-looping Pod has already spent the scheduling round trip.
func Compatible(format weights.Format, engineName string) error {
	prof := BuiltinProfiles().For(engineName)
	if prof.Format.Compatible(format) {
		return nil
	}
	return errs.InvalidArgument("engine %s loads %s weights, not %s",
		prof.Name, prof.Format, format)
}

// EnginesFor lists the engine families that can load a format, for a console
// that wants to offer choices rather than reject them.
func EnginesFor(format weights.Format) []string {
	var out []string
	for _, p := range BuiltinProfiles().All() {
		if p.Format.Compatible(format) {
			out = append(out, p.Name)
		}
	}
	return out
}
