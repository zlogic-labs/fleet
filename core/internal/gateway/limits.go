package gateway

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
)

// The limit tables.
//
// Limits are resolved once at startup into two maps — envelopes by tenant,
// partitions by "tenant/project" — and the limiter reads them per request. The
// alternative, re-parsing a list on every request, is cheap enough to be
// tempting and wrong in a way that shows up under load: it would put string
// splitting on the path that every chat completion takes.
//
// The maps are the *effective* policy, not the declared one. A tenant with no
// entry of its own is present with the server defaults, and a project with no
// entry is absent — meaning "no partition limit of its own", which is
// different from "a partition limited to zero".

// limitTables is the resolved form of LimitConfig.
type limitTables struct {
	// defaults is the envelope for any tenant not named. It is stored rather
	// than applied at each call site so that policies() is the only place that
	// answers "what is this scope limited by" — two places applying the default
	// would be how one of them eventually forgets.
	defaults   authn.Limits
	envelopes  map[string]authn.Limits
	partitions map[string]authn.Limits
}

func (t limitTables) policies(scope ratelimit.Scope) ratelimit.Policies {
	envelope, ok := t.envelopes[scope.Tenant]
	if !ok {
		envelope = t.defaults
	}
	return ratelimit.Policies{
		Envelope:  envelope,
		Partition: t.partitions[scope.Key()],
	}
}

// resolveLimits parses the declared limits into the tables.
//
// Three rules, and each one exists because the alternative is a configuration
// that reads as if it did something it does not:
//
//   - A declared dimension REPLACES the server default; it is not clamped up to
//     it and not merged with it. Zero means "not declared", so `rpm=5,tpm=` is
//     the default token ceiling. An operator who writes `rpm=5` meant 5, and a
//     merge that quietly raised it would be worse than no limit at all.
//   - A project limit above its tenant's envelope is an error, not a clamp. It
//     can never bind — the envelope is checked first and always is — so the
//     operator has written a division that does not divide anything.
//   - A project whose tenant has no envelope is allowed, and inherits the
//     server defaults for one. Requiring an envelope first would make the
//     common deployment — one tenant, a few projects, no explicit ceiling —
//     need three lines of configuration to say what it means.
func resolveLimits(cfg LimitConfig) (limitTables, error) {
	t := limitTables{
		defaults:   authn.Limits{RequestsPerMinute: cfg.RPM, TokensPerMinute: cfg.TPM},
		envelopes:  map[string]authn.Limits{},
		partitions: map[string]authn.Limits{},
	}
	defaults := t.defaults

	for i, spec := range cfg.Tenants {
		scope, limits, err := parseLimitSpec(spec, 1)
		if err != nil {
			return t, fmt.Errorf("rate_limits.tenants[%d] %q: %w", i, spec, err)
		}
		tenant := scope.Normalized().Tenant
		if _, dup := t.envelopes[tenant]; dup {
			return t, fmt.Errorf("rate_limits.tenants[%d] %q: tenant %q is declared twice", i, spec, tenant)
		}
		t.envelopes[tenant] = limits.OrDefaults(defaults)
	}

	// The server defaults are seeded first so a project of an undeclared tenant
	// finds an envelope to sit inside, which resolveLimits documents as legal.
	for i, spec := range cfg.Projects {
		scope, limits, err := parseLimitSpec(spec, 2)
		if err != nil {
			return t, fmt.Errorf("rate_limits.projects[%d] %q: %w", i, spec, err)
		}
		key := scope.Normalized().Key()
		if _, dup := t.partitions[key]; dup {
			return t, fmt.Errorf("rate_limits.projects[%d] %q: project %q is declared twice", i, spec, key)
		}
		envelope, ok := t.envelopes[scope.Normalized().Tenant]
		if !ok {
			envelope = defaults
		}
		if over := widerThan(limits, envelope); over != "" {
			return t, fmt.Errorf(
				"rate_limits.projects[%d] %q: %s is above the tenant's (rpm %d, tpm %d), "+
					"so it could never apply", i, spec, over,
				envelope.RequestsPerMinute, envelope.TokensPerMinute)
		}
		t.partitions[key] = limits
	}
	return t, nil
}

// widerThan names the first dimension in which l exceeds the envelope, or "".
//
// Per dimension rather than whole-set, because the two describe different
// things: a project allowed 1000 requests a minute has not thereby been given
// 1000 tokens a minute.
//
// A zero on the envelope side means unlimited, not zero, so it is skipped. An
// envelope nobody declared is the ordinary case for a single-tenant deployment,
// and comparing a project limit against it would reject every partition of a
// gateway that set no ceiling — leaving no way to limit a project without
// first having to declare a ceiling for the tenant, which is backwards.
func widerThan(l, envelope authn.Limits) string {
	switch {
	case l.RequestsPerMinute > 0 && envelope.RequestsPerMinute > 0 &&
		l.RequestsPerMinute > envelope.RequestsPerMinute:
		return "its request rate"
	case l.TokensPerMinute > 0 && envelope.TokensPerMinute > 0 &&
		l.TokensPerMinute > envelope.TokensPerMinute:
		return "its token rate"
	default:
		return ""
	}
}

// parseLimitSpec reads "name" or "name|rpm=…,tpm=…" into a scope and a policy.
//
// It returns a Scope rather than a bare name so the key and project forms share
// one path: "acme" and "acme/research" both arrive as a scope, and the caller
// decides how many parts it expected.
func parseLimitSpec(spec string, parts int) (ratelimit.Scope, authn.Limits, error) {
	name, suffix, _ := strings.Cut(strings.TrimSpace(spec), "|")
	names, err := authn.Split(name, parts)
	if err != nil {
		return ratelimit.Scope{}, authn.Limits{}, err
	}
	scope := ratelimit.Scope{Tenant: names[0]}
	if parts == 2 {
		scope.Project = names[1]
	}
	if suffix == "" {
		return scope, authn.Limits{}, nil
	}
	limits, err := authn.ParseLimits(suffix)
	if err != nil {
		return ratelimit.Scope{}, authn.Limits{}, err
	}
	return scope, limits, nil
}

// limiterFor builds the limiter the chat handler reserves against.
//
// The policies are read once and cached, so a limit changed in configuration
// does not take effect mid-flight for requests already in the window. That is
// the same contract as the key list itself — configuration is read at startup
// — and it is stated here so that lowering a limit needing a restart is a
// decision rather than a surprise.
//
// The cache is keyed by scope rather than by tenant because a project's policy
// depends on both halves of its name, and two projects of one tenant may be
// limited differently.
//
// The error is returned rather than swallowed. validate has already run the
// same resolution, so in practice it is always nil — but a caller that built a
// Config by hand and skipped validation would otherwise get a gateway running
// with the server defaults and no sign that its declared limits were discarded.
func limiterFor(cfg Config) (*ratelimit.Memory, error) {
	tables, err := resolveLimits(cfg.RateLimits)
	if err != nil {
		return nil, err
	}
	cache := sync.Map{}
	return ratelimit.NewMemory(func(scope ratelimit.Scope) ratelimit.Policies {
		key := scope.Normalized().Key()
		if cached, ok := cache.Load(key); ok {
			return cached.(ratelimit.Policies)
		}
		p := tables.policies(scope.Normalized())
		cache.Store(key, p)
		return p
	}), nil
}

// TrimTenant normalises a tenant name for use as a map key.
//
// Kept next to the limiter rather than inside it so there is exactly one
// definition of "the same tenant", and two spellings of one tenant cannot each
// be handed a full budget. It delegates to ratelimit.Scope.Normalized rather
// than repeating the rule, because a second copy of the normalisation is
// exactly how one tenant ends up with two.
func TrimTenant(s string) string { return ratelimit.Tenant(s).Normalized().Tenant }

func splitList(raw string) []string {
	var out []string
	for _, spec := range strings.Split(raw, ";") {
		if s := strings.TrimSpace(spec); s != "" {
			out = append(out, s)
		}
	}
	return out
}
