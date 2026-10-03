package handler

import (
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
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
	Endpoints func() []engine.Endpoint
}

// NewModels builds the handler over a function that yields the current
// endpoints, rather than over a slice.
//
// The slice form is the bug this replaces: the endpoint set changes when a
// deployment scales, so a handler holding the slice it was constructed with
// serves a model list from whenever the process started. A function defers the
// read to the request, which is the only time it can be correct.
func NewModels(eps func() []engine.Endpoint) *Models { return &Models{Endpoints: eps} }

func (h *Models) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	endpoints := h.Endpoints()
	seen := make(map[string]bool, len(endpoints))
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	now := time.Now().Unix()

	for _, ep := range endpoints {
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
	// Endpoints is a function for the same reason Models takes one: the set
	// moves while the process runs.
	Endpoints func() []engine.Endpoint
	Samples   func() []Sample
	Lic       entitlement.License
	Version   string
	// ControlPlane is where this gateway itself finds the control plane, if it
	// was told. The console is served by the gateway and so already has the
	// answer; hardcoding localhost:8081 in the browser made every page that
	// needs the control plane fail with a connection error on any deployment
	// where the two are not on the same host, and the error looked like a
	// broken product rather than a guessed address.
	ControlPlane string
}

func (h *Fleet) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	granted := make([]string, 0, len(entitlement.AllCapabilities))
	for _, c := range entitlement.AllCapabilities {
		if h.Lic.Granted(c, now) {
			granted = append(granted, string(c))
		}
	}

	endpoints := make([]endpointStatus, 0)
	for _, ep := range h.Endpoints() {
		endpoints = append(endpoints, endpointStatus{
			Model:       ep.Model,
			BaseURL:     ep.BaseURL,
			Replicas:    ep.Replicas,
			Healthy:     ep.Ready(now),
			QueueDepth:  ep.Load.QueueDepth,
			RunningReqs: ep.Load.RunningReqs,
			KVCacheUsed: ep.Load.KVCacheUsed,
		})
	}

	type status struct {
		Edition      string           `json:"edition"`
		Customer     string           `json:"customer,omitempty"`
		ExpiresAt    string           `json:"expiresAt,omitempty"`
		Version      string           `json:"version"`
		ControlPlane string           `json:"controlPlane,omitempty"`
		Capabilities []string         `json:"capabilities"`
		Endpoints    []endpointStatus `json:"endpoints"`
		Recent       []recentSample   `json:"recent"`
	}

	body := status{
		Edition:      string(h.Lic.Edition),
		Customer:     h.Lic.Customer,
		Version:      h.Version,
		ControlPlane: h.ControlPlane,
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
	BaseURL     string  `json:"baseUrl"`
	Replicas    int     `json:"replicas"`
	Healthy     bool    `json:"healthy"`
	QueueDepth  int     `json:"queueDepth"`
	RunningReqs int     `json:"runningRequests"`
	KVCacheUsed float64 `json:"kvCacheUsed"`
}

type recentSample struct {
	// Tenant and Project are omitempty rather than always present: a gateway
	// running without authentication has no scope to report, and an empty
	// string in every row is noise the console would have to filter out.
	Tenant     string `json:"tenant,omitempty"`
	Project    string `json:"project,omitempty"`
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint"`
	Streamed   bool   `json:"streamed"`
	TTFTMs     int64  `json:"ttftMs"`
	DurationMs int64  `json:"durationMs"`
	// DecodeMs is the request's duration minus its time to first token: the
	// part spent producing the answer rather than producing the first token.
	//
	// It is separated because the two answer different questions and conflating
	// them hides the shape of the problem. Time to first token says how long
	// the tenant waited for anything at all; decode says how fast the model was
	// once it had started. A fleet with a slow queue has a large TTFT and a
	// normal decode rate, and an operator cannot tell those apart from the two
	// numbers alone.
	DecodeMs   int64 `json:"decodeMs"`
	UsageKnown bool  `json:"usageKnown"`

	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	CachedTokens     int `json:"cachedTokens"`
	// Estimated marks a request settled from the request rather than from an
	// engine usage field. It is the visible cost of a stream that ended early.
	Estimated bool `json:"estimated"`
}

func recent(samples []Sample) []recentSample {
	out := make([]recentSample, 0, len(samples))
	for i := len(samples) - 1; i >= 0 && len(out) < 16; i-- {
		s := samples[i]
		r := recentSample{
			Tenant:     s.Tenant,
			Project:    s.Project,
			Model:      s.Model,
			Endpoint:   s.Endpoint,
			Streamed:   s.Streamed,
			TTFTMs:     s.TTFT.Milliseconds(),
			DurationMs: s.Duration.Milliseconds(),
			DecodeMs:   decodeMs(s),
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

// decodeMs is the time spent producing the answer rather than the first token.
//
// Clamped at zero: a blocking request can report a duration shorter than its own
// measured time to first token when the two clocks disagree by a millisecond,
// and a negative decode would render as a decode rate of minus infinity, which
// reads as a broken engine rather than as a rounding artefact.
func decodeMs(s Sample) int64 {
	d := s.Duration.Milliseconds() - s.TTFT.Milliseconds()
	if d < 0 {
		return 0
	}
	return d
}
