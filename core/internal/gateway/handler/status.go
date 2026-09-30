package handler

import (
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/engine"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Models serves GET /v1/models.
//
// The list is the union of what the configured upstreams advertise. A model
// with no healthy endpoint is still listed, because a 404 from /v1/models
// reads to a client like "this gateway does not host that model" and is far
// more confusing than an error at call time.
type Models struct {
	Endpoints []engine.Endpoint
}

func NewModels(eps []engine.Endpoint) *Models { return &Models{Endpoints: eps} }

func (h *Models) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	seen := make(map[string]bool, len(h.Endpoints))
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	now := time.Now().Unix()

	for _, ep := range h.Endpoints {
		if seen[ep.Model] {
			continue
		}
		seen[ep.Model] = true
		list.Data = append(list.Data, openai.Model{
			ID:      ep.Model,
			Object:  "model",
			Created: now,
			OwnedBy: ownerOf(ep),
		})
	}
	openai.WriteJSON(w, http.StatusOK, list)
}

func ownerOf(ep engine.Endpoint) string {
	if ep.Labels != nil {
		if owner := ep.Labels["owned_by"]; owner != "" {
			return owner
		}
	}
	return "fleet"
}

// Fleet serves GET /fleet/status, which the console reads.
//
// It is deliberately not part of the OpenAI protocol: clients have no use for
// entitlement data, and keeping it on a separate path means the compatibility
// surface stays exactly what OpenAI specifies.
type Fleet struct {
	Endpoints []engine.Endpoint
	Samples   func() []Sample
	Lic       entitlement.License
	Version   string
}

func (h *Fleet) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	granted := make([]string, 0, len(entitlement.AllCapabilities))
	for _, c := range entitlement.AllCapabilities {
		if h.Lic.Granted(c, now) {
			granted = append(granted, string(c))
		}
	}

	endpoints := make([]endpointStatus, 0, len(h.Endpoints))
	for _, ep := range h.Endpoints {
		endpoints = append(endpoints, endpointStatus{
			Model:       ep.Model,
			BaseURL:     ep.BaseURL,
			Replicas:    ep.Replicas,
			Healthy:     !ep.Load.Stale(now, 2*time.Minute) || ep.Load.UpdatedAt.IsZero(),
			QueueDepth:  ep.Load.QueueDepth,
			RunningReqs: ep.Load.RunningReqs,
			KVCacheUsed: ep.Load.KVCacheUsed,
		})
	}

	type status struct {
		Edition      string           `json:"edition"`
		Customer     string           `json:"customer,omitempty"`
		ExpiresAt    string           `json:"expires_at,omitempty"`
		Version      string           `json:"version"`
		Capabilities []string         `json:"capabilities"`
		Endpoints    []endpointStatus `json:"endpoints"`
		Recent       []recentSample   `json:"recent"`
	}

	body := status{
		Edition:      string(h.Lic.Edition),
		Customer:     h.Lic.Customer,
		Version:      h.Version,
		Capabilities: granted,
		Endpoints:    endpoints,
		Recent:       recent(h.Samples()),
	}
	if !h.Lic.ExpiresAt.IsZero() {
		body.ExpiresAt = h.Lic.ExpiresAt.UTC().Format(time.RFC3339)
	}

	openai.WriteJSON(w, http.StatusOK, body)
}

type endpointStatus struct {
	Model       string  `json:"model"`
	BaseURL     string  `json:"base_url"`
	Replicas    int     `json:"replicas"`
	Healthy     bool    `json:"healthy"`
	QueueDepth  int     `json:"queue_depth"`
	RunningReqs int     `json:"running_requests"`
	KVCacheUsed float64 `json:"kv_cache_used"`
}

type recentSample struct {
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint"`
	Streamed   bool   `json:"streamed"`
	TTFTMs     int64  `json:"ttft_ms"`
	DurationMs int64  `json:"duration_ms"`
	UsageKnown bool   `json:"usage_known"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	// Estimated marks a request settled from the request rather than from an
	// engine usage field. It is the visible cost of a stream that ended early.
	Estimated bool `json:"estimated"`
}

func recent(samples []Sample) []recentSample {
	out := make([]recentSample, 0, len(samples))
	for i := len(samples) - 1; i >= 0 && len(out) < 16; i-- {
		s := samples[i]
		r := recentSample{
			Model:      s.Model,
			Endpoint:   s.Endpoint,
			Streamed:   s.Streamed,
			TTFTMs:     s.TTFT.Milliseconds(),
			DurationMs: s.Duration.Milliseconds(),
			UsageKnown: s.UsageKnown,
			Estimated:  !s.UsageKnown,
		}
		if s.Usage != nil {
			r.PromptTokens = s.Usage.PromptTokens
			r.CompletionTokens = s.Usage.CompletionTokens
			r.CachedTokens = s.Usage.CachedPromptTokens()
		} else {
			// P6: without engine usage, the request is charged what it
			// reserved. Showing it as usage_known=false rather than zeros
			// keeps the ledger honest.
			r.PromptTokens = s.PromptEst
			r.CompletionTokens = s.Requested
		}
		out = append(out, r)
	}
	return out
}
