package gateway

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// A declared limit replaces the server default for that dimension rather than
// being clamped up to it, and a scope left undeclared is simply the default. The
// clamp direction is the part that matters: an operator who writes `rpm=5` and
// gets 100 has a configuration that does not do what it says, which is worse
// than no configuration at all.
func TestDeclaredLimitsReplaceTheServerDefault(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		RateLimits: LimitConfig{
			RPM: 100, TPM: 5000,
			Tenants:  []string{"acme|rpm=50", "globex|rpm=500"},
			Projects: []string{"acme/research|rpm=7"},
		},
	}
	lim := limiterMust(t, cfg)

	// Below the default: honoured as written.
	if got := lim.PolicyFor(ratelimit.Project("acme", "research")).Envelope.RequestsPerMinute; got != 50 {
		t.Errorf("tenant envelope = %d, want 50 as written", got)
	}
	// Above the default: the tenant raised its own ceiling, which it may do.
	if got := lim.PolicyFor(ratelimit.Tenant("globex")).Envelope.RequestsPerMinute; got != 500 {
		t.Errorf("globex envelope = %d, want 500", got)
	}
	// A partition says only rpm, so it stays silent on the token dimension —
	// which means no limit of its own there, NOT the server default. Applying
	// the server default to a partition would silently cap every project at the
	// ceiling meant for tenants nobody declared.
	if got := lim.PolicyFor(ratelimit.Project("acme", "research")).Partition.TokensPerMinute; got != 0 {
		t.Errorf("partition tpm = %d, want 0 (no limit of its own)", got)
	}
	// The partition's own request rate.
	if got := lim.PolicyFor(ratelimit.Project("acme", "research")).Partition.RequestsPerMinute; got != 7 {
		t.Errorf("partition rpm = %d, want 7", got)
	}
	// A tenant nobody declared gets the server defaults.
	unknown := lim.PolicyFor(ratelimit.Tenant("unknown")).Envelope
	if unknown.RequestsPerMinute != 100 || unknown.TokensPerMinute != 5000 {
		t.Errorf("undeclared tenant = %+v, want the server defaults", unknown)
	}
}

// A tenant's envelope must actually bound its projects. If the partition limits
// were merged into one figure instead of checked separately, the tenant would
// get the sum — so this asserts the arithmetic that the two-counter design
// exists to prevent.
func TestProjectLimitsDoNotAddUpToTheEnvelope(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		RateLimits: LimitConfig{
			RPM:      100,
			Tenants:  []string{"acme|rpm=100"},
			Projects: []string{"acme/a|rpm=30", "acme/b|rpm=30", "acme/c|rpm=30"},
		},
	}
	tables, err := resolveLimits(cfg.RateLimits)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Each project has its own 30.
	for _, p := range []string{"a", "b", "c"} {
		if got := tables.policies(ratelimit.Project("acme", p)).Partition.RequestsPerMinute; got != 30 {
			t.Errorf("project %s = %d, want 30", p, got)
		}
	}
	// And the tenant has one envelope of 100, not 90 and not 300.
	if got := tables.policies(ratelimit.Tenant("acme")).Envelope.RequestsPerMinute; got != 100 {
		t.Errorf("envelope = %d, want 100", got)
	}
}

// A project limit above its tenant's envelope can never bind, so an operator
// who wrote one believes they have divided a budget they have not divided.
// Rejecting it at startup is the only point where anyone is still reading the
// configuration.
func TestProjectLimitAboveItsEnvelopeIsRejected(t *testing.T) {
	for _, spec := range []string{
		"acme/research|rpm=5000",   // above the request-rate envelope
		"acme/research|tpm=999999", // above the token envelope
	} {
		cfg := Config{
			Listen: "127.0.0.1:0", MaxBodyMB: 1,
			RateLimits: LimitConfig{
				RPM: 100, TPM: 5000,
				Tenants:  []string{"acme|rpm=100,tpm=5000"},
				Projects: []string{spec},
			},
		}
		if err := cfg.validate(); err == nil {
			t.Errorf("validate accepted %q, want an error: it can never apply", spec)
		}
	}
	// The same numbers the right way round are fine: a partition may equal the
	// envelope, which just means it is not what divides the rest.
	ok := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		RateLimits: LimitConfig{
			RPM: 100, TPM: 5000,
			Tenants:  []string{"acme|rpm=100,tpm=5000"},
			Projects: []string{"acme/research|rpm=100,tpm=5000"},
		},
	}
	if err := ok.validate(); err != nil {
		t.Errorf("validate rejected a partition equal to its envelope: %v", err)
	}
}

// An unlimited envelope is not a zero to exceed. A gateway that declared no
// ceiling must still be able to limit one project, or the only way to get a
// partition limit is to invent a tenant ceiling first — which is backwards, and
// would leave a single-tenant deployment unable to bound anything.
func TestProjectLimitIsAllowedUnderAnUnlimitedEnvelope(t *testing.T) {
	for _, cfg := range []LimitConfig{
		// No envelope and no server default at all.
		{Projects: []string{"acme/research|tpm=200"}},
		// No envelope declared, but the server default is unlimited.
		{Projects: []string{"acme/research|rpm=5,tpm=200"}},
	} {
		if err := (Config{Listen: "127.0.0.1:0", MaxBodyMB: 1, RateLimits: cfg}).validate(); err != nil {
			t.Errorf("validate rejected %+v: %v", cfg, err)
		}
	}
}

// A project of a tenant nobody declared is legal and inherits the server
// defaults for its envelope. Requiring the envelope to be written first would
// make the common deployment — one tenant, a few projects, no explicit ceiling
// — take three lines to say what it means.
func TestProjectOfAnUndeclaredTenantInheritsTheServerDefaults(t *testing.T) {
	tables, err := resolveLimits(LimitConfig{
		RPM: 100, TPM: 5000,
		Projects: []string{"acme/research|rpm=7"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p := tables.policies(ratelimit.Project("acme", "research"))
	if p.Envelope.RequestsPerMinute != 100 || p.Envelope.TokensPerMinute != 5000 {
		t.Errorf("envelope = %+v, want the server defaults", p.Envelope)
	}
	if p.Partition.RequestsPerMinute != 7 {
		t.Errorf("partition = %+v, want rpm 7", p.Partition)
	}
}

// Two spellings of one scope must resolve to one bucket, or the tenant has two
// budgets and only knows about one of them.
func TestDeclaredScopesAreNormalised(t *testing.T) {
	tables, err := resolveLimits(LimitConfig{
		RPM:      100,
		Tenants:  []string{"Acme|rpm=50"},
		Projects: []string{"ACME/Research|rpm=7"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := tables.policies(ratelimit.Project("acme", "research"))
	if got.Envelope.RequestsPerMinute != 50 || got.Partition.RequestsPerMinute != 7 {
		t.Errorf("policies = %+v / %+v, want the entry declared as Acme/Research", got.Envelope, got.Partition)
	}
}

// A duplicate declaration is a configuration that reads as two divisions and
// applies as one. Which of the two wins would depend on map iteration order.
func TestDuplicateLimitDeclarationsAreRejected(t *testing.T) {
	cases := map[string]LimitConfig{
		"duplicate tenant":  {Tenants: []string{"acme|rpm=5", "Acme|rpm=9"}},
		"duplicate project": {Tenants: []string{"acme"}, Projects: []string{"acme/r|rpm=5", "ACME/R|rpm=9"}},
	}
	for name, rl := range cases {
		cfg := Config{Listen: "127.0.0.1:0", MaxBodyMB: 1, RateLimits: rl}
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: validate succeeded, want an error", name)
		}
	}
}

// A malformed limit declaration must fail at startup. Accepting "acme/r" as a
// project and "acme/research" as a tenant would put both in buckets nothing
// reads — a limit that never applies, which looks exactly like one that is not
// being enforced.
func TestMalformedLimitDeclarationsAreRejected(t *testing.T) {
	cases := map[string]LimitConfig{
		"tenant with two parts": {Tenants: []string{"acme/research|rpm=5"}},
		"project with one part": {Projects: []string{"acme|rpm=5"}},
		"empty project name":    {Projects: []string{"acme/|rpm=5"}},
		"unknown limit name":    {Tenants: []string{"acme|rps=5"}},
		"non-numeric limit":     {Tenants: []string{"acme|rpm=lots"}},
	}
	for name, rl := range cases {
		cfg := Config{Listen: "127.0.0.1:0", MaxBodyMB: 1, RateLimits: rl}
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: validate succeeded, want an error", name)
		}
	}
}

// A key spec from before limits moved must be refused at startup, with a
// message that says where they went. Silently ignoring the numbers would leave
// an operator with a budget they believe is in force.
func TestKeySpecsCarryingLimitsFailStartup(t *testing.T) {
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		Auth: AuthConfig{Required: true, Keys: []string{"acme/research/admin|rpm=10"}},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate accepted a key spec carrying limits")
	}
	if got := err.Error(); !contains(got, "rate_limits.tenants") && !contains(got, "rate_limits.projects") {
		t.Errorf("error %q does not say where limits moved", got)
	}
}
