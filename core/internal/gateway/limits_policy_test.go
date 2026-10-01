package gateway

import (
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// The policy is read once per scope and cached, so a configuration that changed
// underneath a running gateway does not alter what in-flight requests release.
// Lowering a limit needs a restart; this is that contract's test, and it says
// so on purpose.
func TestPolicyIsCachedPerScope(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		RateLimits: LimitConfig{RPM: 100, Tenants: []string{"acme|rpm=10"}},
	}
	lim := limiterMust(t, cfg)
	scope := ratelimit.Project("acme", "research")
	if got := lim.PolicyFor(scope).Envelope.RequestsPerMinute; got != 10 {
		t.Fatalf("first read = %d, want 10", got)
	}

	cfg.RateLimits.Tenants = []string{"acme|rpm=9999"}
	other := limiterMust(t, cfg)
	if got := other.PolicyFor(scope).Envelope.RequestsPerMinute; got != 9999 {
		t.Errorf("a fresh limiter = %d, want 9999 — the new configuration must take effect", got)
	}
	if got := lim.PolicyFor(scope).Envelope.RequestsPerMinute; got != 10 {
		t.Errorf("cached policy = %d, want the original 10", got)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func limiterMust(t *testing.T, cfg Config) *ratelimit.Memory {
	t.Helper()
	lim, err := limiterFor(cfg)
	if err != nil {
		t.Fatalf("limiterFor: %v", err)
	}
	return lim
}
