package gateway

import (
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/metrics"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
	"github.com/zlogic-labs/fleet/core/pkg/prom"
)

// The gateway's own numbers, not the ledger's.
//
// Everything here is in-process and resets when the process restarts, which is
// the right division of labour: Prometheus is the durable store for monitoring,
// the ledger is the authority for billing. What this file guards is that the
// series Fleet publishes say what the metric names claim, because a metric that
// means something subtly different is worse than one that is missing.

func newTestObserver() (*metrics.Registry, *observer) {
	reg := metrics.New()
	return reg, newObserver(reg)
}

func record(rec billing.Record) billing.Record {
	rec.Tenant, rec.Project, rec.Model = "acme", "research", "gpt-4o"
	return rec
}

func find(t *testing.T, reg *metrics.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, s := range prom.Parse([]byte(reg.String())) {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if got, ok := s.Label(k); !ok || got != v {
				match = false
				break
			}
		}
		if match {
			return s.Value, true
		}
	}
	return 0, false
}

func TestTokensAreCountedPerKindRatherThanSummed(t *testing.T) {
	// Fresh prompt tokens, cached prompt tokens and reasoning tokens are billed
	// at three different rates. A single summed counter cannot answer "how much
	// of this was cache hits", which is the number that decides whether a
	// prefix-affinity router is earning its keep.
	reg, obs := newTestObserver()
	obs.Served(record(billing.Record{
		Usage: openai.Usage{
			PromptTokens:            1000,
			TotalTokens:             1300,
			CompletionTokens:        300,
			PromptTokensDetails:     &openai.PromptTokensDetails{CachedTokens: 400},
			CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: 200},
		},
		Amount: 1234,
	}), time.Second, 200*time.Millisecond, true)

	for kind, want := range map[string]float64{
		"prompt": 1000, "cached": 400, "fresh": 600, "completion": 300, "reasoning": 200,
	} {
		got, ok := find(t, reg, "fleet_tokens_total", map[string]string{
			"tenant": "acme", "project": "research", "model": "gpt-4o", "kind": kind})
		if !ok {
			t.Errorf("no series for %s tokens", kind)
			continue
		}
		if got != want {
			t.Errorf("%s tokens = %v, want %v", kind, got, want)
		}
	}
	// Fresh is prompt minus cached. Getting this wrong in the other direction
	// bills cache hits at full rate; getting it right but negative bills the
	// tenant for not using the cache.
	fresh, _ := find(t, reg, "fleet_tokens_total", map[string]string{"kind": "fresh"})
	if fresh != 600 {
		t.Errorf("fresh = %v, want 600", fresh)
	}
	if got, _ := find(t, reg, "fleet_spend_micro_total", nil); got != 1234 {
		t.Errorf("spend = %v micro, want 1234", got)
	}
}

func TestAnEstimatedRequestIsCountedSeparately(t *testing.T) {
	// These tokens were not measured, they were assumed from the reservation.
	// An alert on token volume has to be able to say how much of the total is
	// real, or it alerts on a number nobody can act on.
	reg, obs := newTestObserver()
	obs.Served(record(billing.Record{
		UsageSource: billing.SourceReserved,
		Usage:       openai.Usage{PromptTokens: 10, CompletionTokens: 4096, TotalTokens: 4106},
	}), time.Second, -1, false)
	if got, _ := find(t, reg, "fleet_usage_estimated_total", map[string]string{"source": "reserved"}); got != 1 {
		t.Errorf("estimated = %v, want 1", got)
	}

	// A counted answer is measured rather than assumed, and the two are not
	// the same problem: one is an engine to upgrade, the other is a bug.
	reg3, obs3 := newTestObserver()
	obs3.Served(record(billing.Record{
		UsageSource: billing.SourceCounted,
		Usage:       openai.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
	}), time.Second, -1, false)
	if got, _ := find(t, reg3, "fleet_usage_estimated_total", map[string]string{"source": "counted"}); got != 1 {
		t.Errorf("counted = %v, want 1", got)
	}
	if got, _ := find(t, reg3, "fleet_usage_estimated_total", map[string]string{"source": "reserved"}); got != 0 {
		t.Errorf("a counted answer was reported as billed from the reservation: %v", got)
	}

	reg2, obs2 := newTestObserver()
	obs2.Served(record(billing.Record{
		UsageKnown: true,
		Usage:      openai.Usage{PromptTokens: 10, TotalTokens: 12},
	}), time.Second, 30*time.Millisecond, true)
	if got, _ := find(t, reg2, "fleet_usage_estimated_total", nil); got != 0 {
		t.Errorf("a request with a reported usage was counted as estimated: %v", got)
	}
}

func TestANonStreamedRequestReportsNoFirstToken(t *testing.T) {
	// A non-streamed response has one delivery event, so "time to first token"
	// is the whole request. Publishing the duration under that name would put
	// two different quantities on one graph, and the percentile that answers
	// "is this slow to first token" would silently become "is this slow".
	reg, obs := newTestObserver()
	obs.Served(record(billing.Record{}), 2*time.Second, -1, false)
	if _, found := find(t, reg, "fleet_request_first_token_seconds_count", nil); found {
		t.Error("a non-streamed request produced a time-to-first-token observation")
	}
	if got, _ := find(t, reg, "fleet_request_duration_seconds_count", nil); got != 1 {
		t.Errorf("duration count = %v, want 1", got)
	}

	reg2, obs2 := newTestObserver()
	obs2.Served(record(billing.Record{}), 2*time.Second, 150*time.Millisecond, true)
	if got, _ := find(t, reg2, "fleet_request_first_token_seconds_count", nil); got != 1 {
		t.Error("a streamed request produced no time-to-first-token observation")
	}
}

// An endpoint that has never published a load sample is not a broken endpoint.
// llama.cpp without --metrics publishes nothing at all, and every engine is
// briefly un-sampled at startup. Counting those as unhealthy would put a fleet
// of working engines permanently in the red.
func TestAnEndpointWithNoMetricsIsNotAnUnhealthyEndpoint(t *testing.T) {
	reg, obs := newTestObserver()
	obs.publishEndpoints([]engine.Endpoint{
		{ID: "a", Model: "m", Load: engine.Load{UpdatedAt: time.Now(), QueueDepth: 3}},
		{ID: "b", Model: "m", Load: engine.Load{UpdatedAt: time.Now().Add(-10 * time.Minute)}},
		{ID: "c", Model: "m"},
	})
	if got, _ := find(t, reg, "fleet_endpoints", map[string]string{"state": "healthy"}); got != 2 {
		t.Errorf("healthy = %v, want 2 (the reporting one and the silent one)", got)
	}
	if got, _ := find(t, reg, "fleet_endpoints", map[string]string{"state": "unhealthy"}); got != 1 {
		t.Errorf("unhealthy = %v, want 1 (the stale one)", got)
	}
	// The engine that publishes nothing has no gauges at all. A queue depth of
	// zero on it would be a measurement nobody took.
	if _, found := find(t, reg, "fleet_endpoint_queue_depth", map[string]string{"endpoint": "c"}); found {
		t.Error("an engine that publishes no metrics was given a queue depth of zero")
	}
	if got, _ := find(t, reg, "fleet_endpoint_queue_depth", map[string]string{"endpoint": "a"}); got != 3 {
		t.Errorf("queue depth = %v, want 3", got)
	}
}

// A scrape with a bad key has to be visible. Otherwise a fleet whose metrics
// endpoint started rejecting credentials looks identical to a fleet with no
// traffic, and the first is a fixable misconfiguration while the second is
// either an incident or nothing at all.
func TestRefusalsAreCountedByAClosedReason(t *testing.T) {
	reg, obs := newTestObserver()
	obs.Refused("rate_limited")
	obs.Refused("rate_limited")
	obs.Refused("budget_exhausted")
	if got, _ := find(t, reg, "fleet_refused_total", map[string]string{"reason": "rate_limited"}); got != 2 {
		t.Errorf("rate_limited = %v, want 2", got)
	}
	if got, _ := find(t, reg, "fleet_refused_total", map[string]string{"reason": "budget_exhausted"}); got != 1 {
		t.Errorf("budget_exhausted = %v, want 1", got)
	}
	// No tenant label: a refusal can happen before a tenant is known.
	for _, s := range prom.Parse([]byte(reg.String())) {
		if s.Name == "fleet_refused_total" {
			if _, ok := s.Label("tenant"); ok {
				t.Error("fleet_refused_total carries a tenant label")
			}
			break
		}
	}
}

func TestNoSeriesCarriesAKeyID(t *testing.T) {
	// Keys rotate and are issued per integration, so a key label produces
	// series that appear and vanish forever. The ledger records the key.
	reg, obs := newTestObserver()
	obs.Served(record(billing.Record{Endpoint: "e", Amount: 1}), time.Second, time.Millisecond, true)
	obs.Refused("no_endpoint")
	for _, s := range prom.Parse([]byte(reg.String())) {
		if _, ok := s.Label("key"); ok {
			t.Errorf("%s carries a key label", s.Name)
		}
	}
}
