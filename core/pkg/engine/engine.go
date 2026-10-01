// Package engine defines the boundary between Fleet and the software that
// actually runs a model.
//
// Nothing in this package, and nothing above it, may import a vendor SDK. The
// only contract is the OpenAI-compatible HTTP protocol (P1 in
// docs/architecture.md), so swapping vLLM for SGLang, or inserting a custom
// runtime, is a configuration change rather than a code change.
package engine

import (
	"context"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// Endpoint is one addressable replica group serving a model over the
// OpenAI-compatible protocol. A vLLM Pod behind a Service is one Endpoint;
// ten of them behind a Service is one Endpoint with Replicas=10.
type Endpoint struct {
	ID      string
	Model   string
	BaseURL string
	// Replicas is the number of identical instances behind BaseURL. Routing
	// needs it to weight load, and health needs it to avoid declaring the
	// whole group down when a single instance restarts.
	Replicas int
	// Labels carry scheduling attributes set by the controller: gpu.model,
	// cluster, zone, deployment.
	Labels map[string]string
	// Load is refreshed out of band. The request path must never scrape a
	// metric inline; a slow scrape would show up as gateway latency.
	Load Load
}

// String implements fmt.Stringer with the identity a human needs in a log line.
func (e Endpoint) String() string { return e.Model + "@" + e.BaseURL }

// Load is the scheduling signal set, sampled by a poller from the engine's
// own metrics. Fields are deliberately the same three signals AIBrix's KPA
// uses: request_count, kv_cache_size, queue depth. GPU utilisation is not
// among them, because an LLM decode loop pins the GPU at 100% by design and
// using it as a scaling signal amplifies every request into an autoscaling
// event.
type Load struct {
	QueueDepth  int
	RunningReqs int
	KVCacheUsed float64 // 0..1
	UpdatedAt   time.Time
}

// Stale reports whether the sample is too old to act on. A frozen reading
// must not be treated as a healthy one.
func (l Load) Stale(now time.Time, maxAge time.Duration) bool {
	return l.UpdatedAt.IsZero() || now.Sub(l.UpdatedAt) > maxAge
}

// Capability describes what a running engine actually supports, as opposed to
// what its documentation claims. vLLM renames CLI flags and Prometheus metric
// names between releases, so the controller must feature-detect (P4) instead
// of hard-coding a version check.
type Capability struct {
	Engine  string
	Version string
	// MaxModelLen is the engine's context window as configured for this
	// deployment, which can be lower than the model's own limit.
	MaxModelLen int
	// Tokenize reports whether a /tokenize extension is served. It is the
	// cheap path to exact prompt counts and to reconciling our own estimates.
	// Its absence is normal, not a fault: llama-server has no such endpoint.
	Tokenize bool
	Tools    bool
	JSONMode bool
	// ServedModels is what /v1/models reports, which may be more than one
	// when several checkpoints share a process.
	ServedModels []string
	// Profile is the engine family the endpoint was identified as, and Format
	// what weights it was found to serve. Both are echoed into status so the
	// console can explain a deployment instead of just colouring it.
	Profile      string
	WeightFormat weights.Format
	// MetricsAvailable records whether autoscaling can be driven from this
	// engine. False is a normal, supported state: llama-server publishes no
	// vLLM metric set, and a deployment using it scales by replica count.
	MetricsAvailable bool
	// Capacity is what one replica can hold at once. Zero values mean the
	// engine published no capacity gauge, which is a fact about the engine
	// rather than a fault — llama.cpp does not report one.
	Capacity Capacity
	ProbedAt time.Time
}

// Adapter probes a running engine.
//
// There is one implementation of this interface per protocol, not per vendor,
// because there is exactly one protocol (P1). Engine diversity lives in
// Profile, which is data. Anyone tempted to add a second Adapter to
// accommodate a second engine is about to re-create the vendor coupling P1
// exists to prevent — the answer is a Profile, or a new probe on this one.
type Adapter interface {
	// Name identifies the protocol, not the engine family.
	Name() string
	// Probe reports capabilities. It must not fail hard when an optional
	// capability is missing; only an unreachable engine is a hard failure.
	// The profile says which optional endpoints to look for; the adapter says
	// how to ask.
	Probe(ctx context.Context, ep Endpoint, p Profile) (Capability, error)
	// Models lists the model identifiers the endpoint serves.
	Models(ctx context.Context, ep Endpoint) ([]string, error)
}
