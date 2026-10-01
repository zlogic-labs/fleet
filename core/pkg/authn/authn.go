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
	// Tenant is who is charged. Several projects and several API keys may share
	// one tenant, which is what makes it possible to rotate a key or split a
	// budget without moving a balance.
	Tenant string
	// Project is the partition inside the tenant this key spends from. It is
	// what a per-project rate limit is keyed on and what the ledger attributes
	// the spend to, so it is resolved at authentication rather than taken from
	// the request body — a client that named its own project could charge
	// whichever budget it liked.
	Project string
	// KeyID names the key that was presented, for logs and for revoking one
	// key without touching the tenant.
	KeyID string
	// Labels are the key's own attributes, passed through to billing so a
	// cost can be attributed to a team rather than only to a tenant.
	Labels map[string]string
}

// Limits is a rate limit policy.
//
// It lives here rather than in ratelimit because the configuration parser,
// the control plane and the limiter all have to agree on one shape, and a
// second definition would let them drift: the parser would accept `rpm` and the
// limiter would read something else.
//
// A limit is a property of a scope — a tenant or a project — and never of a
// key. Keys are credentials: they rotate and they leak, so a budget attached to
// one would have to be reissued along with it.
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// Unlimited reports whether the limits permit anything at all.
func (l Limits) Unlimited() bool { return l.RequestsPerMinute <= 0 && l.TokensPerMinute <= 0 }

// OrDefaults fills in each dimension l left unstated from d.
//
// A zero means "not declared", not "unlimited", which is why this is a fill-in
// and not a merge. A scope that says only `rpm` still inherits the server's
// token ceiling, because a configuration that sets one number and silently
// leaves the other unbounded does not do what it says.
func (l Limits) OrDefaults(d Limits) Limits {
	if l.RequestsPerMinute <= 0 {
		l.RequestsPerMinute = d.RequestsPerMinute
	}
	if l.TokensPerMinute <= 0 {
		l.TokensPerMinute = d.TokensPerMinute
	}
	return l
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

// ScopeFromContext is the caller's tenant and project in the canonical spelling
// the limiter keys on.
//
// Both parts are normalised here rather than by each caller, because a limiter
// that keyed "Acme" and "acme" separately would hand one tenant two full
// budgets — the same reason the same normalisation has to happen on the
// configuration side, which is why this mirrors ratelimit.Scope.Normalized
// rather than inventing a third spelling.
//
// An unauthenticated caller gets an empty scope rather than a placeholder like
// "anonymous": a single shared bucket would let one unauthenticated caller
// exhaust the limit that every anonymous caller shares, which turns a disabled
// authenticator into a denial of service. Anonymous traffic is instead
// unlimited by default, which is the honest reading — nothing is being charged,
// so there is nothing to ration.
func ScopeFromContext(ctx context.Context) (tenant, project string) {
	p, ok := FromContext(ctx)
	if !ok {
		return "", ""
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	tenant, project = norm(p.Tenant), norm(p.Project)
	if tenant == "" {
		// A project with no tenant names a partition of nothing. Dropping it
		// keeps the pair one the limiter can key on, rather than leaving a
		// project bucket that no envelope can bound.
		project = ""
	}
	return tenant, project
}
