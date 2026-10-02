package gateway

import (
	"sync"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
)

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
//
// A database, when configured, supplies the policies instead of the config
// tables, and it also holds the counters.
//
// The counters are the reason this is not only about policies. An in-process
// counter enforces the limit per replica: a gateway running three pods admits
// three times the declared rate, and nothing reports a problem because each
// pod is individually correct. With a database the limit is the limit.
//
// Without a database the limiter falls back to the in-process one, which is
// right for a laptop and wrong for a deployment with more than one gateway
// replica. That is a deployment decision rather than a silent default, so the
// fallback is only reachable by a configuration with no database at all.
func limiterFor(cfg Config, db *sqlstore.DB) (ratelimit.Limiter, error) {
	tables, err := resolveLimits(cfg.RateLimits)
	if err != nil {
		return nil, err
	}

	// The policy source owns its own context and its own per-call deadline.
	// It cannot be given one from here: this function returns, and a context
	// with a deferred cancel is already cancelled by the time the first request
	// looks a policy up — which silently turns every database lookup into a
	// failure, and a failed lookup returns the server defaults, so every tenant
	// becomes unlimited.
	cache := &policyCache{}
	var src *sqlstore.PolicySource
	if db != nil {
		src = sqlstore.NewPolicySource(db, tables.defaults)
	}
	policyFor := func(scope ratelimit.Scope) ratelimit.Policies {
		return cache.get(scope, func(s ratelimit.Scope) ratelimit.Policies {
			if src == nil {
				return tables.policies(s)
			}
			return src.Policies(s)
		})
	}
	if db != nil {
		return sqlstore.NewRateLimiter(db, policyFor), nil
	}
	return ratelimit.NewMemory(policyFor), nil
}

// policyCache memoises one scope's policy.
//
// Load-then-Store, not LoadOrStore: two concurrent first-requests for one scope
// may both read the database and both store, and the loser discards a value
// equal to the winner's. Coalescing them onto a single in-flight read needs a
// per-key lock and buys one query, at the cost of a much more complicated
// structure, on a path that runs once per scope per process.
type policyCache struct {
	m sync.Map
}

func (c *policyCache) get(scope ratelimit.Scope, load func(ratelimit.Scope) ratelimit.Policies) ratelimit.Policies {
	key := scope.Normalized().Key()
	if cached, ok := c.m.Load(key); ok {
		return cached.(ratelimit.Policies)
	}
	p := load(scope.Normalized())
	c.m.Store(key, p)
	return p
}
