// Package engine defines the boundary between Fleet and the software that
// actually runs a model.
//
// Nothing in this package, and nothing above it, may import a vendor SDK. The
// only contract is the OpenAI-compatible HTTP protocol (P1 in
// docs/architecture.md), so swapping vLLM for SGLang, or inserting a custom
// runtime, is a configuration change rather than a code change.
package engine

import (
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

// StaleAfter is how long a load sample stays actionable.
//
// Two minutes against a one-minute scrape interval (a 15-second refresh taking
// every fourth pass) is two missed scrapes of slack, and short enough that an
// engine which stopped answering stops being routed to while an operator is
// still watching a dashboard. The margin has to be a multiple of the scrape
// interval rather than a round number: a constant expressed as "two minutes"
// invites someone to compare it against the refresh period and conclude it is
// generous.
const StaleAfter = 2 * time.Minute

// Ready reports whether the endpoint is answering.
//
// A sample that was never taken is not evidence of failure: an engine whose
// profile declares no metrics publishes nothing at all, and every engine is
// briefly un-sampled at startup and after a scrape error. So an absent sample
// counts as ready and a *stale* one does not.
//
// The distinction lives here rather than at each call site because two views of
// "is this endpoint answering" that disagree are worse than either: the
// console would show a green endpoint that the router has already stopped
// sending traffic to.
func (e Endpoint) Ready(now time.Time) bool {
	return e.Load.UpdatedAt.IsZero() || !e.Load.Stale(now, StaleAfter)
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

// There was an Adapter interface here, one implementation per protocol, and it
// is gone for the same reason the adapter registry went before it: there is one
// protocol (P1), so there was one implementation, so the interface promised a
// second implementation that the architecture forbids. Anyone reaching for it
// to accommodate a second engine is about to re-create the vendor coupling P1
// exists to prevent -- the answer is a Profile, which is a literal, or a new
// probe inside pkg/engine/openai.
//
// What probing costs, now stated plainly: nothing in the gateway calls it. The
// operator probes, in fleet-serving, and reports the result through the
// inventory contract; that is where the endpoint's Capability comes from. This
// package's probe is the same logic kept next to the Profile it reads, for the
// autoscaler that will need it and for anyone verifying a profile by hand.
