package gateway

import (
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/metrics"
)

// What Fleet publishes about itself.
//
// Written by hand rather than generated, and that is the point: the set of
// series is closed, the label names are declared once, and adding a metric is
// an edit to this file where a reader can see the whole surface.
//
// Two rules about the labels:
//
//   - No key id. Tenants are few and named by a human; keys are issued
//     per-integration and rotate, so a key label produces series that appear
//     and disappear forever and a time series that is mostly holes. The ledger
//     records the key, which is where per-credential answers belong.
//   - No error message. The text of an error changes with the code that wrote
//     it; the outcome name is a closed set and is safe to group by.

// Latency buckets in seconds.
//
// Spanning 10ms to 30s: an embedding is milliseconds, a prefill on a long
// context is seconds, and a request that waited for a queue is longer still.
// The gaps are wide because a resolution finer than this costs series count
// without answering a question anybody asks of a fleet gateway.
var latencyBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type observer struct {
	reg *metrics.Registry

	requests  *metrics.Counter
	duration  *metrics.Histogram
	ttft      *metrics.Histogram
	tokens    *metrics.Counter
	spend     *metrics.Counter
	estimated *metrics.Counter
	refused   *metrics.Counter
	endpoints *metrics.Gauge
	queue     *metrics.Gauge
	running   *metrics.Gauge
	kvCache   *metrics.Gauge
	buildInfo *metrics.Gauge
}

// newObserver declares every family, once, with its help text.
func newObserver(reg *metrics.Registry) *observer {
	o := &observer{reg: reg}
	o.requests = reg.Counter("fleet_requests_total",
		"Requests finished, by the model that served them and how it ended.",
		"tenant", "project", "model", "outcome")
	o.duration = reg.Histogram("fleet_request_duration_seconds",
		"Wall clock seconds from accepting the request to the last byte of the response.",
		latencyBuckets, "tenant", "project", "model")
	o.ttft = reg.Histogram("fleet_request_first_token_seconds",
		"Seconds to the first token. Streaming requests only; absent is not zero.",
		latencyBuckets, "tenant", "project", "model")
	o.tokens = reg.Counter("fleet_tokens_total",
		"Tokens billed, by which side of the bill they are on.",
		"tenant", "project", "model", "kind")
	o.spend = reg.Counter("fleet_spend_micro_total",
		"Millionths of a currency unit, in the price book's currency.",
		"tenant", "project", "model")
	o.estimated = reg.Counter("fleet_usage_estimated_total",
		"Requests settled without an engine-reported usage, by what they were billed on instead.",
		"tenant", "project", "model", "source")
	o.refused = reg.Counter("fleet_refused_total",
		"Requests Fleet turned away, by reason. No tenant label: a refusal can happen before there is one.",
		"reason")
	o.endpoints = reg.Gauge("fleet_endpoints",
		"Endpoints the router can currently use, by whether they answered the last probe.", "state")
	o.queue = reg.Gauge("fleet_endpoint_queue_depth",
		"Requests waiting on each endpoint, as last scraped from the engine.", "endpoint", "model")
	o.running = reg.Gauge("fleet_endpoint_running_requests",
		"Requests executing on each endpoint, as last scraped from the engine.", "endpoint", "model")
	o.kvCache = reg.Gauge("fleet_endpoint_kv_cache_used",
		"Fraction of the KV cache in use, as last scraped from the engine.", "endpoint", "model")
	o.buildInfo = reg.Gauge("fleet_build_info",
		"Always 1. The labels carry the version and edition; a gauge is the shape that has somewhere to put them.",
		"version", "edition")
	return o
}

// served records a request that reached an engine.
//
// Tokens are recorded per kind rather than as a total because fresh prompt
// tokens, cached prompt tokens and reasoning tokens are billed at three
// different rates. A single sum is the number nobody can act on.
func (o *observer) served(rec billing.Record, duration, ttft float64, streamed bool) {
	tenant, project := rec.Tenant, projectName(rec)
	o.requests.Inc(tenant, project, rec.Model, "ok")
	o.duration.Observe(duration, tenant, project, rec.Model)
	if streamed && ttft >= 0 {
		o.ttft.Observe(ttft, tenant, project, rec.Model)
	}
	u := rec.Usage
	o.tokens.Add(float64(u.PromptTokens), tenant, project, rec.Model, "prompt")
	o.tokens.Add(float64(u.CachedPromptTokens()), tenant, project, rec.Model, "cached")
	o.tokens.Add(float64(u.FreshPromptTokens()), tenant, project, rec.Model, "fresh")
	o.tokens.Add(float64(u.CompletionTokens), tenant, project, rec.Model, "completion")
	o.tokens.Add(float64(u.ReasoningTokens()), tenant, project, rec.Model, "reasoning")
	o.spend.Add(float64(rec.Amount), tenant, project, rec.Model)
	if !rec.UsageKnown {
		// Counted separately rather than folded into the token counters: these
		// tokens are an estimate, and an alert on tokens must be able to say
		// how much of the total was real. Split by what it was measured from,
		// because "the gateway counted the answer" and "we billed the ceiling"
		// are different problems with different fixes.
		o.estimated.Inc(tenant, project, rec.Model, string(rec.UsageSource))
	}
}

// refused records a turn-away.
//
// The reasons themselves live in the handler package, as handler.Reason*
// constants, and are not repeated here. They used to be: this file carried its
// own copy, nine constants with four extras, referenced by nothing, and both
// comments claimed to be the closed set. A second copy of a closed set is not
// a closed set — adding a reason to the real one silently does nothing here,
// and nothing reads here to notice.
func (o *observer) refuse(reason string) { o.refused.Inc(reason) }

// publishEndpoints publishes the routable set and each endpoint's last scraped load.
//
// The load gauges are refreshed from the same sample the router reads, so a
// dashboard and a routing decision cannot disagree about a queue depth.
func (o *observer) publishEndpoints(set []engine.Endpoint) {
	healthy := 0
	now := time.Now()
	for _, ep := range set {
		// The same freshness test the status page uses. Two views of "is this
		// endpoint answering" that disagree are worse than one.
		if !ep.Ready(now) {
			continue
		}
		healthy++
		// Only published for an endpoint that publishes them. An engine whose
		// profile declares no metrics leaves the gauge absent rather than at
		// zero, because a zero queue depth on an engine that never reported one
		// is a fabricated measurement and an autoscaler will believe it.
		if ep.Load.UpdatedAt.IsZero() {
			continue
		}
		o.queue.Set(float64(ep.Load.QueueDepth), ep.ID, ep.Model)
		o.running.Set(float64(ep.Load.RunningReqs), ep.ID, ep.Model)
		o.kvCache.Set(float64(ep.Load.KVCacheUsed), ep.ID, ep.Model)
	}
	o.endpoints.Set(float64(healthy), "healthy")
	o.endpoints.Set(float64(len(set)-healthy), "unhealthy")
}

func (o *observer) version(v, edition string) { o.buildInfo.Set(1, v, edition) }

// The adapter the handlers hold. It satisfies handler.Observer without the
// handler package importing pkg/metrics, so a test can pass nil and a handler
// can be constructed without a registry.

// Served implements handler.Observer.
func (o *observer) Served(rec billing.Record, duration, ttft time.Duration, streamed bool) {
	// A negative ttft means "this request had no first token", which is a
	// different statement from a first token of zero.
	first := -1.0
	if ttft >= 0 {
		first = ttft.Seconds()
	}
	o.served(rec, duration.Seconds(), first, streamed)
}

// Refused implements handler.Observer.
func (o *observer) Refused(reason string) { o.refuse(reason) }
