// Package authn identifies the caller and says what they may do.
//
// It answers two questions the gateway needs before anything else, and both
// answers come from one place: who is this, and what are they entitled to. The
// principal is resolved once per request and then travels on the context, so
// no downstream layer re-parses a credential or re-derives a permission.
package authn

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Principal is the authenticated caller.
//
// It is a value rather than an interface because everything downstream wants
// the same handful of facts, and an interface here would be a nil check in
// every handler — the one mistake that turns into a panic on the request path.
type Principal struct {
	// Tenant is who is charged. Several API keys may share one tenant, which
	// is what makes it possible to rotate a key without moving a balance.
	Tenant string
	// KeyID names the key that was presented, for logs and for revoking one
	// key without touching the tenant.
	KeyID string
	// RateLimit overrides the server default when the key declares its own.
	// Zero means "use the server's", which keeps the common case — one policy
	// for everyone — free of per-key bookkeeping.
	RateLimit Limits
	// Labels are the key's own attributes, passed through to billing so a
	// cost can be attributed to a team rather than only to a tenant.
	Labels map[string]string
}

// Limits is a per-key override of the server's rate limit policy.
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// Set reports whether the key declared its own policy.
func (l Limits) Set() bool { return l.RequestsPerMinute > 0 || l.TokensPerMinute > 0 }

// Unlimited reports whether the limits permit anything at all.
func (l Limits) Unlimited() bool { return l.RequestsPerMinute <= 0 && l.TokensPerMinute <= 0 }

// OrDefaults fills in each dimension l left unstated from d.
//
// A zero means "not declared", not "unlimited", which is why this is a fill-in
// and not a merge. The gateway applies it to a key's declared limits, so a key
// that says only `rpm` still inherits the server's token ceiling — and a key
// that says nothing at all is simply the server's policy.
func (l Limits) OrDefaults(d Limits) Limits {
	if l.RequestsPerMinute <= 0 {
		l.RequestsPerMinute = d.RequestsPerMinute
	}
	if l.TokensPerMinute <= 0 {
		l.TokensPerMinute = d.TokensPerMinute
	}
	return l
}

// Tightest returns the most restrictive of two sets of limits, per dimension.
//
// Per dimension rather than whole-set, because the two halves describe
// different things: a tenant handed a token ceiling has not thereby been handed
// an unlimited request rate.
//
// It is the merge the gateway applies across a tenant's keys, and taking the
// tightest rather than the loosest is the part that matters. Limits exist to
// protect the shared GPUs, so if two keys of one tenant declared 1 and 1000
// requests per minute, honouring the loosest would make the tighter declaration
// decorative — the tenant would raise its own ceiling by issuing another key.
// The cost is that a deliberately low limit on one key also throttles its
// siblings, which is the honest reading: the keys share one bucket because they
// share one tenant, and the tenant is one bill.
func (l Limits) Tightest(other Limits) Limits {
	return Limits{
		RequestsPerMinute: minPositive(l.RequestsPerMinute, other.RequestsPerMinute),
		TokensPerMinute:   minPositive(l.TokensPerMinute, other.TokensPerMinute),
	}
}

// minPositive returns the smaller of two values, treating zero as "unlimited"
// rather than as the smallest possible value. A dimension nothing declared must
// not clamp a dimension something else did.
func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// Granted reports whether the caller's licence covers a capability.
//
// The licence is the server's, not the key's: a key cannot grant what the
// deployment does not have. That is what makes GET /fleet/status a fact rather
// than a claim, and what stops a stale page badge from unlocking anything.
func (p Principal) Granted(c entitlement.Capability, now time.Time, lic entitlement.License) bool {
	return lic.Granted(c, now)
}

// KeyStore resolves a presented credential.
//
// It returns a zero Principal and ok=false for an unknown key rather than an
// error, because "not a key we know" is the ordinary path for a typo and is
// not worth a different message from "expired".
type KeyStore interface {
	// Lookup returns the principal for a key.
	Lookup(ctx context.Context, key string) (Principal, bool)
	// Revoke invalidates one key. Absent from the interface on purpose: v1 has
	// no key management API, and a method nothing calls is a promise nobody
	// has checked.
}

// Lister exposes the registered keys.
//
// It is separate from KeyStore because only the rate limiter needs the whole
// list, and folding it into KeyStore would require the Postgres backend to
// answer a question about every key on a path that runs for every request.
type Lister interface {
	Keys() []Principal
}

// Authenticator turns a request into a Principal.
type Authenticator struct {
	Store KeyStore
	Lic   entitlement.License
}

// Authenticate resolves the credential on r.
//
// An empty store means authentication is disabled, and that is stated out loud
// rather than inferred: a gateway with no keys is a developer's laptop, and a
// deployment that silently stopped checking keys would look identical to one
// that is working.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if a == nil || a.Store == nil {
		return Principal{}, nil
	}
	key := BearerToken(r)
	if key == "" {
		return Principal{}, errs.Unauthenticated(
			"provide an API key in the Authorization header as 'Bearer <key>'")
	}
	p, ok := a.Store.Lookup(ctx, key)
	if !ok {
		// One message for both an unknown key and a revoked one: telling them
		// apart tells an attacker which of the two they got right.
		return Principal{}, errs.Unauthenticated("invalid API key")
	}
	return p, nil
}

// BearerToken reads the credential, accepting the two header spellings real
// clients use.
//
// The `x-api-key` form is Anthropic's, and clients written against one vendor
// and pointed at another gateway should not have to be rewritten before the
// request is even accepted. Both are stripped before the request is forwarded,
// so a tenant key never reaches an engine that logs request headers.
func BearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if key, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(key)
		}
		// A bare token with no scheme is what several SDKs send.
		if !strings.Contains(v, " ") {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// StripCredentials removes the caller's credential from a request that is about
// to be forwarded.
//
// This is not defensive tidiness. An engine that logs request headers would
// otherwise write one tenant's key into another tenant's operational logs, and
// a key is a bearer credential for the whole tenant.
func StripCredentials(r *http.Request) {
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
}

// ── context plumbing ────────────────────────────────────────────

type ctxKey struct{}

var principalKey ctxKey

// WithPrincipal stores p on ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// FromContext returns the authenticated caller.
//
// A missing principal is not an error here. Handlers that require one are
// already behind the authentication middleware, and a handler that checks
// again would be checking something that cannot have changed — while a handler
// that panics on a nil here turns an ordinary anonymous request into a 500.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// MustPrincipal returns the caller or fails the request.
//
// Used where continuing anonymously would be wrong — chiefly settlement, which
// has to charge somebody.
func MustPrincipal(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok || p.Tenant == "" {
		return Principal{}, errs.Unauthenticated("this endpoint requires an authenticated caller")
	}
	return p, nil
}

// TenantFromContext is the caller's tenant in the canonical spelling the
// limiter keys on, or "" for an anonymous request.
//
// Empty rather than a placeholder like "anonymous": a single shared bucket
// would let one unauthenticated caller exhaust the limit that every anonymous
// caller shares, which turns a disabled authenticator into a denial of
// service. Anonymous traffic is instead unlimited by default, which is the
// honest reading — nothing is being charged, so there is nothing to ration.
func TenantFromContext(ctx context.Context) string {
	p, ok := FromContext(ctx)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(p.Tenant))
}
