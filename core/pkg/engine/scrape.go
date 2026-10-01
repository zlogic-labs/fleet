package engine

import (
	"context"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/prom"
)

// Capacity is what one replica can hold at once.
//
// These numbers are the input to three separate decisions — how to price a
// replica, what an autoscaler aims for, and whether a context length is even
// legal — and they come from the engine rather than from Fleet's arithmetic
// because only the engine knows how it laid out its KV blocks. Fleet computing
// them would mean reimplementing vLLM's memory profiler and then drifting from
// it on the next release.
type Capacity struct {
	// KVTokens is the KV cache size in tokens, per replica.
	KVTokens int64
	// MaxConcurrency is the engine's own estimate of how many requests at the
	// deployment's context length run at once before prefill stalls. It is a
	// float because vLLM computes it as a ratio, and because at short context
	// lengths the useful answer really is fractional in spirit even when the
	// scheduler floors it.
	MaxConcurrency float64
	// MaxModelLen is the context window in force for this deployment, which
	// can be lower than the model's own limit.
	MaxModelLen int
	// At is when this was read. A capacity from an hour ago describes an
	// engine that has since been restarted with different settings.
	At time.Time
}

// ConcurrencyAt estimates how many requests of n tokens each fit at once.
//
// This is the number that makes cost planning possible. A replica with 400k
// tokens of KV cache selling 1M-token requests has a MaxConcurrency below 1,
// and every request it accepts queues behind the one already running — the
// deployment is charged GPU-hours whether it serves one request a minute or a
// thousand.
func (c Capacity) ConcurrencyAt(tokensPerRequest int) float64 {
	if c.KVTokens <= 0 || tokensPerRequest <= 0 {
		return 0
	}
	return float64(c.KVTokens) / float64(tokensPerRequest)
}

// Scraper reads an engine's Prometheus endpoint.
type Scraper interface {
	// Scrape parses the engine's metrics into load signals and, when the
	// profile declares a capacity gauge, capacity.
	Scrape(ctx context.Context, ep Endpoint, p Profile) (Load, Capacity, error)
}

// ScrapeClient fetches the exposition body.
//
// It is a separate type from ProbeClient for the same reason: /metrics on a
// busy engine is a large body, and probing capability must never pay that cost
// on a control-plane loop.
type ScrapeClient struct {
	hc *metricsClient
}

// NewScrapeClient bounds a single scrape. Short by default: a metrics endpoint
// that takes seconds is a metrics endpoint whose numbers are already stale by
// the time they are read.
func NewScrapeClient(timeout time.Duration) *ScrapeClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ScrapeClient{hc: newMetricsClient(timeout)}
}

var _ Scraper = (*ScrapeClient)(nil)

// Scrape reads /metrics once.
//
// The load signals and the capacity come from the same body on purpose: they
// are a consistent snapshot of one engine, and two scrapes can disagree with
// each other in a way that makes the numbers nonsense.
func (s *ScrapeClient) Scrape(ctx context.Context, ep Endpoint, p Profile) (Load, Capacity, error) {
	body, err := s.hc.get(ctx, ep.BaseURL, p.Metrics.Path)
	if err != nil {
		return Load{}, Capacity{}, err
	}
	samples := prom.Parse(body)
	now := time.Now()

	return signals(samples, p.Metrics, now), capacity(samples, p.Metrics, now), nil
}

// signals extracts the scheduling series.
//
// A series the engine does not publish leaves its field at zero, which is the
// correct value for a queue and the wrong one for a percentage. The distinction
// is kept by the caller through MetricsAvailable rather than smuggled in here:
// an engine with no metrics at all reports Available false and no consumer reads
// these zeros as measurements.
func signals(samples []prom.Sample, spec MetricsSpec, now time.Time) Load {
	out := Load{UpdatedAt: now}
	if v, ok := prom.Value(samples, spec.Series[SignalQueueDepth]); ok {
		out.QueueDepth = int(v)
	}
	if v, ok := prom.Value(samples, spec.Series[SignalRunningReqs]); ok {
		out.RunningReqs = int(v)
	}
	if v, ok := prom.Value(samples, spec.Series[SignalKVCacheUsed]); ok {
		out.KVCacheUsed = v
	}
	return out
}

// capacity extracts the capacity labels from the info gauge.
func capacity(samples []prom.Sample, spec MetricsSpec, now time.Time) Capacity {
	if !spec.CapacityAvailable() {
		return Capacity{}
	}
	s, ok := prom.Find(samples, spec.InfoGauge)
	if !ok {
		return Capacity{}
	}
	out := Capacity{At: now}
	if v, ok := prom.IntLabel(s, spec.KVTokensLabel); ok {
		out.KVTokens = v
	}
	if v, ok := prom.FloatLabel(s, spec.MaxConcurrencyLabel); ok {
		out.MaxConcurrency = v
	}
	return out
}

// errNoMetrics is what a scrape reports when the engine has no /metrics.
var errNoMetrics = errs.NotFound("this engine publishes no Prometheus metrics")
