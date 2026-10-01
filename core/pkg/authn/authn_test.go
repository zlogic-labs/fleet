package authn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

func TestBearerTokenAcceptsTheFormsRealClientsSend(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"Bearer scheme", map[string]string{"Authorization": "Bearer sk-abc"}, "sk-abc"},
		{"bare token", map[string]string{"Authorization": "sk-abc"}, "sk-abc"},
		// Anthropic's header, because a client written against one vendor and
		// pointed at another gateway should not have to be rewritten before its
		// request is even accepted.
		{"x-api-key", map[string]string{"x-api-key": "sk-abc"}, "sk-abc"},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			if got := BearerToken(r); got != tc.want {
				t.Errorf("BearerToken = %q, want %q", got, tc.want)
			}
		})
	}
}

// A tenant key reaching an engine that logs request headers leaks one tenant's
// credential into another's operational logs, and that cannot be undone. So the
// strip happens before forwarding, not in the engine.
func TestStripCredentialsRemovesBothHeaderForms(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-tenant")
	r.Header.Set("x-api-key", "sk-tenant")

	StripCredentials(r)

	if got := r.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it removed", got)
	}
	if got := r.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q, want it removed", got)
	}
}

func TestUnknownKeyAndMissingKeyAreIndistinguishable(t *testing.T) {
	store := NewMemory()
	store.Put("real-key", Principal{Tenant: "acme", KeyID: "real-key"}, time.Time{})
	a := &Authenticator{Store: store}

	ctx := context.Background()
	if _, err := a.Authenticate(ctx, reqWithKey("real-key")); err != nil {
		t.Fatalf("a registered key must authenticate: %v", err)
	}

	_, missing := a.Authenticate(ctx, reqWithKey(""))
	_, unknown := a.Authenticate(ctx, reqWithKey("not-a-key"))
	if missing == nil || unknown == nil {
		t.Fatal("both an absent and an unknown key must be rejected")
	}
	// Both failures answer 401 with one code. The messages may differ — telling
	// an operator that no header arrived is more useful than "invalid" — but
	// neither may echo key material, and the difference must not reveal that
	// some *other* key exists.
	if errs.CodeOf(missing) != errs.CodeOf(unknown) {
		t.Errorf("codes differ: %q vs %q, so one failure maps to a different status",
			errs.CodeOf(missing), errs.CodeOf(unknown))
	}
	for _, err := range []error{missing, unknown} {
		if strings.Contains(err.Error(), "not-a-key") || strings.Contains(err.Error(), "real-key") {
			t.Errorf("error echoes key material: %q", err.Error())
		}
	}
	if errs.KindOf(unknown) != errs.KindUnauthenticated {
		t.Errorf("kind = %v, want unauthenticated", errs.KindOf(unknown))
	}
}

func TestExpiredKeyIsRejected(t *testing.T) {
	store := NewMemory()
	store.Put("old", Principal{Tenant: "acme"}, time.Now().Add(-time.Minute))
	a := &Authenticator{Store: store}

	if _, err := a.Authenticate(context.Background(), reqWithKey("old")); err == nil {
		t.Fatal("an expired key must not authenticate")
	}
	// The clock is a parameter so this is not a test that has to sleep.
	if (MemoryKey{ExpiresAt: time.Now().Add(time.Minute)}).Valid(time.Now()) != true {
		t.Error("a future expiry must be valid")
	}
}

// A disabled authenticator has to mean "no authentication", not "deny all". The
// two must be distinguishable: a developer running dev.sh with no keys and an
// operator who fat-fingered the key list should not both get a gateway that
// refuses everything, for opposite reasons.
func TestNilStoreMeansNoAuthentication(t *testing.T) {
	a := &Authenticator{}
	p, err := a.Authenticate(context.Background(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err != nil {
		t.Fatalf("a nil store must not reject: %v", err)
	}
	if p.Tenant != "" {
		t.Errorf("tenant = %q, want empty for an anonymous caller", p.Tenant)
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	store := NewMemory()
	store.Put("leaked", Principal{Tenant: "acme"}, time.Time{})
	store.Revoke("leaked")

	if _, ok := store.Lookup(context.Background(), "leaked"); ok {
		t.Error("a revoked key must not resolve")
	}
}

func TestParsePrincipalReadsLimits(t *testing.T) {
	p, err := ParsePrincipal("acme/team-a|rpm=10,tpm=1000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Tenant != "acme" || p.KeyID != "team-a" {
		t.Errorf("parsed %+v", p)
	}
	if p.RateLimit.RequestsPerMinute != 10 || p.RateLimit.TokensPerMinute != 1000 {
		t.Errorf("limits = %+v", p.RateLimit)
	}
	if !p.RateLimit.Set() {
		t.Error("Set() = false after limits were declared")
	}
}

// A typo in a key spec must fail at startup, not become a tenant named
// something nobody intended.
func TestParsePrincipalRejectsMalformedSpecs(t *testing.T) {
	for _, spec := range []string{"", "acme", "/key", "acme/", "acme/key|bogus=1", "acme/key|rpm=x"} {
		if _, err := ParsePrincipal(spec); err == nil {
			t.Errorf("ParsePrincipal(%q) succeeded, want an error", spec)
		}
	}
}

func TestTenantFromContextNormalises(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Tenant: "  Acme  "})
	// Two spellings of one tenant must reach the same limiter bucket, or each
	// gets a full allowance.
	if got := TenantFromContext(ctx); got != "acme" {
		t.Errorf("TenantFromContext = %q, want %q", got, "acme")
	}
	if got := TenantFromContext(context.Background()); got != "" {
		t.Errorf("anonymous tenant = %q, want empty", got)
	}
}

func reqWithKey(key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}
