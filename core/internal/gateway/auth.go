package gateway

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// authenticate resolves the caller and puts them on the context.
//
// It runs before the body is read on purpose. A request with no valid key costs
// nothing to reject, and rejecting it after tokenising the prompt would mean an
// unauthenticated caller can make the gateway do work — which is the shape of a
// free denial-of-service tool.
func authenticate(auth *authn.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := auth.Authenticate(r.Context(), r)
			if err != nil {
				unauthorized(w, err)
				return
			}
			// Stripped here rather than at the proxy: by the time a handler
			// forgets, the credential has already been forwarded to an engine
			// that logs request headers, and a tenant key in another tenant's
			// logs is a credential leak that cannot be undone.
			authn.StripCredentials(r)
			next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
		})
	}
}

// requireCapability rejects a caller the licence does not cover.
//
// It is a separate middleware rather than a check inside a handler because the
// answer is the same for every route that uses it, and a handler that forgets
// to ask would expose a paid capability for free — the one failure an edition
// boundary exists to prevent.
func requireCapability(auth *authn.Authenticator, c entitlement.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := authn.FromContext(r.Context())
			if !p.Granted(c, time.Now(), auth.Lic) {
				openai.WriteError(w, errs.PermissionDenied(
					"this capability (%s) is not in your edition", c))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter, err error) {
	// RFC 6750 says a 401 must advertise how to authenticate. An SDK that
	// reads the header retries with a key; one that only reads the body gives
	// up, and telling it what header it needed is the difference between a
	// recoverable error and a support ticket.
	w.Header().Set("WWW-Authenticate", `Bearer realm="fleet"`)
	openai.WriteError(w, err)
}

// limiterFor builds the limiter the chat handler reserves against.
//
// Two rules, and they are not the same rule:
//
//   - A declared limit REPLACES the server default for that dimension. Zero
//     means the key said nothing about it, so the default stands. The key list
//     is operator configuration, so there is nothing to defend against by
//     clamping it — the operator writing `rpm=5` meant rpm 5, and a merge that
//     quietly raised it to the server's 100 would be a config that does not do
//     what it says.
//   - Across a tenant's keys, the TIGHTEST wins, because the keys share one
//     bucket. Taking the loosest would let a tenant raise its own ceiling by
//     issuing a second, more generous key, which makes every declared limit
//     advisory. See authn.Limits.Tightest.
//
// A per-tenant policy is read on first use and kept for the life of the
// process, so a key rotated after startup cannot change an established tenant's
// limits mid-flight. Lowering a limit therefore needs a restart — the same
// contract as the key list itself, and stated here so it is a decision rather
// than a surprise.
func limiterFor(cfg Config, store authn.Lister) *ratelimit.Memory {
	defaults := ratelimit.Policy{
		RequestsPerMinute: cfg.RateLimits.RPM,
		TokensPerMinute:   cfg.RateLimits.TPM,
	}
	policies := sync.Map{}
	return ratelimit.NewMemory(func(tenant string) ratelimit.Policy {
		if cached, ok := policies.Load(tenant); ok {
			return cached.(ratelimit.Policy)
		}
		p := declaredLimits(tenant, store).OrDefaults(defaults)
		policies.Store(tenant, p)
		return p
	})
}

// declaredLimits returns the limits a tenant's keys declare in aggregate,
// taking the tightest.
//
// The tightest, because the keys share one bucket. Taking the loosest would let
// a tenant raise its own ceiling by issuing a second, more generous key, which
// makes every declared limit advisory. See authn.Limits.Tightest.
func declaredLimits(tenant string, store authn.Lister) ratelimit.Policy {
	if store == nil {
		return ratelimit.Policy{}
	}
	out := ratelimit.Policy{}
	first := true
	for _, k := range store.Keys() {
		if TrimTenant(k.Tenant) != tenant {
			continue
		}
		// The first key initialises rather than merging: minPositive treats zero
		// as unlimited, so starting from a zero Policy would drop the first
		// key's limits entirely.
		if first {
			out, first = k.RateLimit, false
			continue
		}
		out = out.Tightest(k.RateLimit)
	}
	return out
}

// TrimTenant normalises a tenant name for use as a map key.
//
// Kept next to the limiter rather than inside it so there is exactly one
// definition of "the same tenant", and two spellings of one tenant cannot each
// be handed a full limit. The chat handler goes through
// authn.TenantFromContext, which applies the same normalisation, so the two
// spellings share one bucket.
func TrimTenant(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
