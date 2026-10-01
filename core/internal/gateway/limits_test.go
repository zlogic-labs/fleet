package gateway

import (
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
)

// A declared limit replaces the server default for that dimension, and across a
// tenant's keys the tightest wins. Both halves matter: a key that silently got
// clamped up to the default would be a config that does not do what it says,
// and a tenant that could raise its own ceiling by adding a second key would
// make every declared limit advisory.
func TestKeyLimitsReplaceTheDefaultAndTheTightestKeyWins(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		RateLimits: LimitConfig{RPM: 100, TPM: 5000},
	}

	// Two keys straddling the default: the tenant is held to the tighter one.
	both := authn.NewMemory()
	mustPut(t, both, "loose", "acme/loose|rpm=500")
	mustPut(t, both, "quiet", "acme/quiet|rpm=5")
	if got := limiterFor(cfg, both).PolicyFor("acme"); got.RequestsPerMinute != 5 {
		t.Errorf("policy = %+v, want rpm 5 — the tightest key of the tenant", got)
	}

	// One key below the default is honoured as written, not raised to it.
	quiet := authn.NewMemory()
	mustPut(t, quiet, "one", "acme/one|rpm=5")
	if got := limiterFor(cfg, quiet).PolicyFor("acme"); got.RequestsPerMinute != 5 {
		t.Errorf("policy = %+v, want rpm 5 as written", got)
	}

	// One key above the default raises the ceiling.
	high := authn.NewMemory()
	mustPut(t, high, "one", "acme/one|rpm=500")
	if got := limiterFor(cfg, high).PolicyFor("acme"); got.RequestsPerMinute != 500 {
		t.Errorf("policy = %+v, want rpm 500", got)
	}

	// A key that declares only rpm still inherits the token ceiling, rather
	// than falling back to unlimited on the dimension it stayed quiet about.
	mixed := authn.NewMemory()
	mustPut(t, mixed, "one", "acme/one|rpm=7")
	if got := limiterFor(cfg, mixed).PolicyFor("acme"); got.TokensPerMinute != 5000 {
		t.Errorf("policy = %+v, want the inherited tpm 5000", got)
	}

	// A key that declares nothing is simply the server's policy.
	plain := authn.NewMemory()
	mustPut(t, plain, "plain", "acme/plain")
	got := limiterFor(cfg, plain).PolicyFor("acme")
	if got.RequestsPerMinute != 100 || got.TokensPerMinute != 5000 {
		t.Errorf("policy = %+v, want the server defaults", got)
	}
}

// The policy is read once per tenant and cached, so a key rotated after startup
// cannot change an established tenant's limits mid-flight. Lowering a limit
// needs a restart; this is that contract's test, and it says so on purpose.
func TestPolicyIsCachedPerTenant(t *testing.T) {
	store := authn.NewMemory()
	mustPut(t, store, "admin", "acme/admin|rpm=10")

	cfg := Config{Listen: "127.0.0.1:0", MaxBodyMB: 1, RateLimits: LimitConfig{RPM: 100}}
	lim := limiterFor(cfg, store)
	if got := lim.PolicyFor("acme"); got.RequestsPerMinute != 10 {
		t.Fatalf("first read = %+v, want rpm 10", got)
	}

	mustPut(t, store, "admin", "acme/admin|rpm=9999")
	if got := lim.PolicyFor("acme"); got.RequestsPerMinute != 10 {
		t.Errorf("policy = %+v after a key change, want the cached 10", got)
	}
}

func mustPut(t *testing.T, store *authn.Memory, key, spec string) {
	t.Helper()
	p, err := authn.ParsePrincipal(spec)
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	store.Put(key, p, time.Time{})
}
