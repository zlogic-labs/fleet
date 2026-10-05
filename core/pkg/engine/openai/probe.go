package openai

import (
	"context"
	"errors"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Probe reports what the endpoint can do.
//
// Only an unreachable or unhealthy endpoint is a hard failure. Every other
// check is best-effort and its absence is recorded in the returned Capability,
// which is what the controller publishes as a status condition. That split
// matters: an engine that serves chat but not /tokenize is fully usable, and
// failing the whole probe would make the platform look broken.
//
// The profile decides which optional endpoints are worth asking about. This is
// what keeps engine diversity as data: llama-cpp has no tokenize candidates, so
// it is never probed for one, and a new engine is a new Profile rather than a
// new Adapter.
func (a *Adapter) Probe(ctx context.Context, ep engine.Endpoint, p engine.Profile) (engine.Capability, error) {
	cap := engine.Capability{
		Engine:           ep.Model,
		Profile:          p.Name,
		WeightFormat:     p.Format,
		MetricsAvailable: p.Metrics.Available(),
		ProbedAt:         time.Now(),
	}

	if err := a.probeHealth(ctx, ep, p); err != nil {
		return cap, err
	}

	// Soft checks. A nil result leaves the field false, which is a fact about
	// the engine rather than a failure of the platform.
	if models, err := a.Models(ctx, ep); err == nil {
		cap.ServedModels = models
	}
	if cap.MaxModelLen, cap.Tokenize = a.probeTokenize(ctx, ep, p); cap.Tokenize {
		cap.Engine = ep.Model
	}
	if v, ok := a.probeVersion(ctx, ep, p); ok {
		cap.Version = v
	}
	// Capacity last, and softly. It costs one metrics scrape, and an engine
	// that will not serve metrics must not fail the probe that already
	// succeeded: chat still works, and a deployment with no published capacity
	// is a plan to make, not an outage.
	if p.Metrics.CapacityAvailable() {
		if _, c, err := engine.NewScrapeClient(0).Scrape(ctx, ep, p); err == nil {
			cap.Capacity = c
		}
	}
	if cap.Capacity.MaxModelLen == 0 {
		cap.Capacity.MaxModelLen = cap.MaxModelLen
	}
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
//
// It tries each candidate in the profile and passes when any of them answers.
// One path per engine would make a rename look like a stuck image pull, and
// that failure is very hard to diagnose from the outside.
func (a *Adapter) probeHealth(ctx context.Context, ep engine.Endpoint, p engine.Profile) error {
	if len(p.Health) == 0 {
		return errs.Internal(nil)
	}
	var lastErr error
	for _, path := range p.Health {
		u, err := resolve(ep.BaseURL, path)
		if err != nil {
			return err
		}
		// An engine mid-load answers 503, which must be reported as not-ready
		// rather than skipped: trying the next candidate would let a 503 on
		// the real path be masked by a 404 on a wrong one.
		if err := a.client.Do(ctx, "GET", u, nil, nil); err == nil {
			return nil
		} else if !isNotSupported(err) {
			return errs.Upstream(err, "health check failed for %s", ep)
		}
		lastErr = err
	}
	return errs.Upstream(lastErr, "no health endpoint answered for %s (tried %v)", ep, p.Health)
}

// probeTokenize detects a /tokenize extension, which is the cheap route to
// exact prompt counts and to reconciling our own estimates.
//
// Returns the engine's configured context window, which is the value actually
// wanted: a deployment's window is a scheduling decision, not a property of
// the weights.
func (a *Adapter) probeTokenize(ctx context.Context, ep engine.Endpoint, p engine.Profile) (int, bool) {
	for _, path := range p.Tokenize {
		u, err := resolve(ep.BaseURL, path)
		if err != nil {
			continue
		}
		var resp struct {
			Count       int   `json:"count"`
			MaxModelLen int   `json:"max_model_len"`
			Tokens      []int `json:"tokens"`
		}
		// A deliberately trivial prompt: the response carries the configured
		// context window, which is what the caller needs from it.
		in := map[string]string{"prompt": "ping"}
		if err := a.client.Do(ctx, "POST", u, in, &resp); err != nil {
			continue
		}
		return resp.MaxModelLen, true
	}
	return 0, false
}

// probeVersion reads the engine build, from whichever candidate answers.
// Optional throughout: a missing version string must not block a deployment,
// and an unknown build is better than a permanently unready one.
func (a *Adapter) probeVersion(ctx context.Context, ep engine.Endpoint, p engine.Profile) (string, bool) {
	for _, path := range p.Version {
		u, err := resolve(ep.BaseURL, path)
		if err != nil {
			continue
		}
		// Every field this needs, in one struct. llama-server nests the build
		// under build_info while vLLM reports a flat version, and a decoder
		// that only knew the flat shape would silently produce an empty
		// version for every llama.cpp deployment — the kind of quiet gap P4
		// exists to close, where the endpoint answers and Fleet learns nothing.
		var resp struct {
			Version    string `json:"version"`
			Build      string `json:"build"`
			BuildInfo  string `json:"build_info"`
			ModelAlias string `json:"model_alias"`
			TotalSlots int    `json:"total_slots"`
		}
		if err := a.client.Do(ctx, "GET", u, nil, &resp); err != nil {
			continue
		}
		for _, candidate := range []string{resp.Version, resp.BuildInfo, resp.Build} {
			if candidate != "" {
				return candidate, true
			}
		}
		// The endpoint answered but declared no build. That is still a
		// successful probe: the field simply stays unset.
		return "", true
	}
	return "", false
}

// isNotSupported distinguishes "this path does not exist" from "this endpoint
// is unhealthy". Only the first justifies trying the next candidate; a 503
// means the engine is mid-load and must be reported as not ready.
func isNotSupported(err error) bool { return errors.Is(err, errNotSupported) }
