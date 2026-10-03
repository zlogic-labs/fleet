package gateway

import (
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// Authentication off must mean no authentication, not deny-all, or dev.sh
// cannot be used at all.
func TestNoAuthConfigServesAnonymousTraffic(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})
	if w := post(h, "", req); w.Code != http.StatusOK {
		t.Errorf("anonymous request: %d, want 200", w.Code)
	}
}

// A malformed key spec must stop the process, not become a tenant named after
// the operator's typo.
func TestBadKeySpecFailsStartup(t *testing.T) {
	_, _, err := Build(Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true, Keys: []string{"missing-the-slash"}},
	}, nil, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err == nil {
		t.Fatal("a malformed key spec must fail startup")
	}
	if !strings.Contains(err.Error(), "auth.keys[0]") {
		t.Errorf("error %q does not name which key is wrong", err)
	}
}

// A database is somewhere to look a key up; it is not a configured secret.
// Asking whether auth is required and asking whether a key is being ignored
// are different questions, and reusing one answer for both made "point Fleet
// at Postgres, leave auth off" unstartable. That is the shape of a
// single-tenant private deployment, so it has to start.
func TestDatabaseWithAuthOffStarts(t *testing.T) {
	c := Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Database:  DatabaseConfig{URL: "postgres://localhost/fleet"},
	}
	if err := c.validateAuth(true); err != nil {
		t.Errorf("database with auth off must start: %v", err)
	}
}

// The other direction, because the fix could have been to drop the check: a key
// written into the config while auth is off really is silently discarded, and
// that is the shape of a security setting someone believes is on.
func TestConfiguredKeysWithAuthOffStillFail(t *testing.T) {
	c := Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Database:  DatabaseConfig{URL: "postgres://localhost/fleet"},
		Auth:      AuthConfig{Keys: []string{"acme/research/admin"}},
	}
	err := c.validateAuth(true)
	if err == nil || !strings.Contains(err.Error(), "auth.required is false") {
		t.Errorf("a configured key with auth off must be refused, got %v", err)
	}
}

// Auth required with nowhere to look a key up is the outage that looks like a
// working service, so it still has to be refused.
func TestAuthRequiredWithNoSourceOfKeysStillFails(t *testing.T) {
	c := Config{Listen: "127.0.0.1:0", MaxBodyMB: 1, Auth: AuthConfig{Required: true}}
	if err := c.validateAuth(false); err == nil {
		t.Error("auth required with no keys and no database must fail startup")
	}
}

// Two spellings of one scope must not each get a full allowance, and the
// engine must never see either spelling's credential — fakeEngine fails the test
// if an Authorization or x-api-key header reaches it.
func TestTenantNormalisationAndCredentialStrippingEndToEnd(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth: AuthConfig{Required: true, Keys: []string{"Acme/research/admin", "acme/research/admin-2"}},
		RateLimits: LimitConfig{
			RPM:     1000,
			Tenants: []string{"Acme|rpm=1"},
		},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})
	if w := post(h, "Acme/research/admin", req); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	// The same tenant written differently: the declaration normalised to
	// "acme", and so must the request's scope, or this gets a second budget.
	if w := post(h, "acme/research/admin-2", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("second: %d, want 429 — 'Acme' and 'acme' got separate buckets", w.Code)
	}
}
