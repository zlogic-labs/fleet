package openai

import (
	"context"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Name identifies the adapter. It matches the engine family a FleetDeployment
// would name, so a deployment declaring an engine with no dedicated adapter
// falls through to this one.
func (a *Adapter) Name() string { return "openai-compatible" }

// Probe reports what the endpoint can do.
//
// Only an unreachable or unhealthy endpoint is a hard failure. Every other
// check is best-effort and its absence is recorded in the returned Capability,
// which is what the controller publishes as a status condition. That split
// matters: an engine that serves chat but not /tokenize is fully usable, and
// failing the whole probe would make the platform look broken.
func (a *Adapter) Probe(ctx context.Context, ep engine.Endpoint) (engine.Capability, error) {
	cap := engine.Capability{
		Engine:   a.Name(),
		ProbedAt: time.Now(),
	}

	if err := a.probeHealth(ctx, ep); err != nil {
		return cap, err
	}

	// Soft checks.
	if models, err := a.Models(ctx, ep); err == nil {
		cap.ServedModels = models
	}
	a.probeTokenize(ctx, ep, &cap)
	a.probeVersion(ctx, ep, &cap)

	return cap, nil
}

// Models returns the identifiers /v1/models reports.
func (a *Adapter) Models(ctx context.Context, ep engine.Endpoint) ([]string, error) {
	u, err := resolve(ep.BaseURL, "/v1/models")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := a.client.Do(ctx, "GET", u, nil, &resp); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Data))
	for _, d := range resp.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// probeHealth is the only hard gate. Engines use 503 while the model is still
// loading, so a non-200 here means "not ready", which is a fact the scheduler
// acts on rather than an error the platform surfaces.
func (a *Adapter) probeHealth(ctx context.Context, ep engine.Endpoint) error {
	u, err := resolve(ep.BaseURL, "/health")
	if err != nil {
		return err
	}
	if err := a.client.Do(ctx, "GET", u, nil, nil); err != nil {
		return errs.Upstream(err, "health check failed for %s", ep)
	}
	return nil
}

// probeTokenize detects the /tokenize extension. It is the cheap route to
// exact prompt counts and to reconciling our own estimates, so a deployment
// that lacks it needs a note in its status.
func (a *Adapter) probeTokenize(ctx context.Context, ep engine.Endpoint, cap *engine.Capability) {
	u, err := resolve(ep.BaseURL, "/tokenize")
	if err != nil {
		return
	}
	var resp struct {
		Count       int   `json:"count"`
		MaxModelLen int   `json:"max_model_len"`
		Tokens      []int `json:"tokens"`
	}
	// A deliberately trivial prompt: the response carries the engine's
	// configured context window, which is the value we actually need.
	in := map[string]string{"prompt": "ping"}
	if err := a.client.Do(ctx, "POST", u, in, &resp); err != nil {
		return
	}
	cap.Tokenize = true
	cap.MaxModelLen = resp.MaxModelLen
}

// probeVersion reads the engine build. vLLM serves /version; the check is
// skipped silently for runtimes that do not, since a missing version string
// must not block a deployment.
func (a *Adapter) probeVersion(ctx context.Context, ep engine.Endpoint, cap *engine.Capability) {
	u, err := resolve(ep.BaseURL, "/version")
	if err != nil {
		return
	}
	var resp struct {
		Version string `json:"version"`
	}
	if err := a.client.Do(ctx, "GET", u, nil, &resp); err == nil {
		cap.Version = resp.Version
	}
}
