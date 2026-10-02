package gateway

import (
	"fmt"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
)

// Startup validation.
//
// Separate from loading because every check here is a statement about the
// configuration as a whole, and none of them can be attributed to a single
// request. The gateway refuses to start rather than serve traffic it knows is
// misconfigured: an auth setting nobody intended is an outage that looks like
// a working service to health checks.

// validate reports the first way this configuration cannot work.
//
// First, not all — an operator fixing a config file should correct one thing at
// a time, and a list of twelve errors reads as twelve unrelated problems.
func (c Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is empty")
	}
	if c.MaxBodyMB <= 0 {
		return fmt.Errorf("max_body_mb must be positive, got %d", c.MaxBodyMB)
	}
	if c.ControlPlane.Every < 0 {
		return fmt.Errorf("control_plane.refresh_every must not be negative")
	}
	if err := c.validateAuth(); err != nil {
		return err
	}
	// The index, not the spec. A config with twenty keys and one typo is
	// found by position; quoting the offending string leaves the operator
	// counting lines.
	for i, spec := range c.Auth.Keys {
		if _, err := authn.ParsePrincipal(spec); err != nil {
			return fmt.Errorf("auth.keys[%d] %q: %w", i, spec, err)
		}
	}
	// Resolved here rather than at first request, because the one error it can
	// produce — a project limit above its tenant's envelope — is a statement
	// about the configuration as a whole and cannot be attributed to a request.
	if _, err := resolveLimits(c.RateLimits); err != nil {
		return err
	}
	return c.validateUpstreams()
}

// validateAuth checks that authentication can actually succeed.
//
// The two rules that matter are about what the operator believes, not about
// what the process does. Required with no keys refuses every request, which
// looks like an outage rather than a misconfiguration. Keys with required off
// parses them and then ignores them, which is the shape of a security setting
// someone believes is on.
//
// A database counts as a source of keys, so "required with no keys" is only an
// error when there is nowhere at all to look one up. Without this the obvious
// configuration — point Fleet at Postgres, let tenants live in it — would be
// rejected by the very check that exists to catch a typo.
func (c Config) validateAuth() error {
	haveKeys := len(c.Auth.Keys) > 0 || c.Database.URL != ""
	if c.Auth.Required && !haveKeys {
		return fmt.Errorf("auth.required is set but there are no keys: set auth.keys or database.url, " +
			"or every request would be rejected")
	}
	if !c.Auth.Required && haveKeys {
		return fmt.Errorf("keys are configured but auth.required is false: they would be ignored")
	}
	return nil
}

func (c Config) validateUpstreams() error {
	seen := make(map[string]bool, len(c.Upstreams))
	for i, up := range c.Upstreams {
		if up.BaseURL == "" {
			return fmt.Errorf("upstreams[%d]: base_url is required", i)
		}
		if up.Model == "" {
			return fmt.Errorf("upstreams[%d]: model is required", i)
		}
		if up.ID == "" {
			// Deriving the id from the URL keeps single-upstream configs to
			// two lines, which is how most people first run this.
			up.ID = up.Model + "@" + up.BaseURL
		}
		// Two endpoints with one id would both answer to the same name in
		// /v1/models and collide in the routing ring, so the second would
		// silently never be picked.
		if seen[up.ID] {
			return fmt.Errorf("upstreams[%d]: duplicate id %q", i, up.ID)
		}
		seen[up.ID] = true
	}
	return nil
}
