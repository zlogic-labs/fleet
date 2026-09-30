package render

import (
	"strconv"
)

// VLLM renders a FleetDeployment as a vLLM server.
//
// The image is a default, not a promise: pinning it in code would make every
// upgrade a source change, and vLLM's own release cadence is what should
// decide when to move. A deployment overrides it with spec.image.
type VLLM struct {
	image string
}

// NewVLLM returns the vLLM renderer, using defaultImage when it is not empty.
func NewVLLM(defaultImage string) *VLLM { return &VLLM{image: defaultImage} }

func (v *VLLM) Name() string { return "vllm" }

func (v *VLLM) Image() string {
	if v.image != "" {
		return v.image
	}
	return "vllm/vllm-openai:latest"
}

// Command renders the vLLM invocation.
//
// --served-model-name is not optional. vLLM reports a model name derived from
// the weights path, and a gateway that receives a client asking for "qwen-7b"
// and a response saying "weights/qwen-7b" cannot decide whether the replica
// is the right one: the name clients send and the name the engine reports have
// to be the same string.
func (v *VLLM) Command(in Input) ([]string, []string) {
	spec := in.Deployment.Spec
	name := selectorFor(in.Model)

	// vLLM is pointed at the directory: it reads config.json and the shards
	// beside it, and which shards exist is a question about the repository
	// rather than something an operator states.
	args := []string{
		"--model", weightsDir(in),
		"--served-model-name", name,
		"--port", "8000",
		"--host", "0.0.0.0",
	}
	// A parallelism of one is the default and passing it as zero is a hard
	// error, so the flags are only stated when they are not one.
	if n := spec.TensorParallelSize; n > 1 {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(int(n)))
	}
	if n := spec.PipelineParallelSize; n > 1 {
		args = append(args, "--pipeline-parallel-size", strconv.Itoa(int(n)))
	}
	if n := spec.ContextLength; n > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(int(n)))
	}
	return []string{"vllm", "serve"}, append(args, flags(spec.EngineOptions)...)
}

// Accelerators asks for whole NVIDIA devices, one per parallel rank.
//
// A request rather than only a limit, so two TP=1 replicas cannot both be
// placed on a node holding one card: the scheduler counts requests, and the
// second replica then stays pending instead of oversubscribing a device.
func (v *VLLM) Accelerators(in Input) (string, int32) {
	return "nvidia.com/gpu", in.Deployment.Spec.GPUPerReplica()
}

func (v *VLLM) HealthPath(in Input) string {
	return firstCandidate(in.Profile.Health)
}

// LlamaCPP renders a FleetDeployment as a llama.cpp server.
type LlamaCPP struct {
	image string
}

// NewLlamaCPP returns the llama.cpp renderer, using defaultImage when set.
func NewLlamaCPP(defaultImage string) *LlamaCPP { return &LlamaCPP{image: defaultImage} }

func (l *LlamaCPP) Name() string { return "llama-cpp" }

func (l *LlamaCPP) Image() string {
	if l.image != "" {
		return l.image
	}
	return "ghcr.io/ggml-org/llama.cpp:server"
}

// Command renders the llama-server invocation.
//
// The one fact worth stating is --alias. Without it llama-server serves a
// model whose id is the absolute path of the GGUF file, verified against
// b11146: both /v1/models and every response body report
// "D:/models/qwen2.5-0.5b-instruct-q4_k_m.gguf". A gateway that routes by
// model name then has nothing to match, so the alias is set explicitly.
func (l *LlamaCPP) Command(in Input) ([]string, []string) {
	spec := in.Deployment.Spec
	name := selectorFor(in.Model)

	// llama-server is pointed at the file: it takes one GGUF path and has no
	// notion of a repository, so the directory plus the chosen file is the
	// whole of it.
	args := []string{
		"--model", weightsPath(in, MainGGUF(in.Model.Status.Files)),
		"--alias", name,
		"--host", "0.0.0.0",
		"--port", "8000",
		// One slot per replica. The default is several, which on a CPU engine
		// means several conversations sharing a small KV cache, and a request
		// queueing behind four others it will never overtake.
		"-np", strconv.Itoa(int(maxInt32(spec.Replicas, 1))),
	}
	// A GGUF context is a fixed allocation. Not stating it lets the engine
	// pick from the host it happens to be on, so two identical deployments on
	// two nodes allocate two different KV caches and answer differently.
	if n := spec.ContextLength; n > 0 {
		args = append(args, "-c", strconv.Itoa(int(n)))
	}
	return []string{"llama-server"}, append(args, flags(spec.EngineOptions)...)
}

// Accelerators requests no device.
//
// llama.cpp runs on a CPU, and a pod that asks for nvidia.com/gpu on a cluster
// with no GPU is a pod that stays Pending forever while the operator reports
// "scheduling". Asking for nothing is what lets the same CRD run on a
// workstation and on an A100 node.
func (l *LlamaCPP) Accelerators(in Input) (string, int32) { return "", 0 }

func (l *LlamaCPP) HealthPath(in Input) string {
	return firstCandidate(in.Profile.Health)
}

// Builtin returns the renderers Fleet ships.
func Builtin() *Registry {
	r := NewRegistry()
	r.Register(NewVLLM(""))
	r.Register(NewLlamaCPP(""))
	return r
}
