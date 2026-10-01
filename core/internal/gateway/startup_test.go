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
	}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err == nil {
		t.Fatal("a malformed key spec must fail startup")
	}
	if !strings.Contains(err.Error(), "auth.keys[0]") {
		t.Errorf("error %q does not name which key is wrong", err)
	}
}

// Two spellings of one tenant must not each get a full allowance, and the
// engine must never see either spelling's credential — fakeEngine fails the test
// if an Authorization or x-api-key header reaches it.
func TestTenantNormalisationAndCredentialStrippingEndToEnd(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"Acme/admin|rpm=1", "acme/admin-2|rpm=1000"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})
	if w := post(h, "admin", req); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	if w := post(h, "admin-2", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("second: %d, want 429 — 'Acme' and 'acme' got separate buckets", w.Code)
	}
}
