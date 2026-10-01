package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The reservation is the request's upper bound, so a tenant at its limit is
// turned away before the engine is touched at all.
func TestRateLimitRefusesAndAdvertisesRetryAfter(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:       AuthConfig{Required: true, Keys: []string{"acme/research/admin"}},
		RateLimits: LimitConfig{Tenants: []string{"acme|rpm=2"}},
		Upstreams:  []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	for i := 1; i <= 2; i++ {
		if w := post(h, "acme/research/admin", req); w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, w.Code)
		}
	}
	w := post(h, "acme/research/admin", req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After; a client without it retries in a tight loop")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("engine saw %d calls, want 2 — a refused request still reached the GPU", got)
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body is not an OpenAI envelope: %v", err)
	}
	if env.Error.Type == "" {
		t.Error("429 body carries no error type")
	}
	// The refusal names the scope that ran out, or a caller has no way to tell
	// its own budget from one project's.
	if !strings.Contains(w.Body.String(), "acme") {
		t.Errorf("429 body %s does not name the tenant", w.Body)
	}
}

// The invariant the two counters exist for, asserted over HTTP rather than on
// the limiter: a project limit on its own is arithmetic the tenant performs. Ten
// projects at 300 requests a minute is 3000, and no per-project number would
// ever have bound. Here one project spends its whole allowance and a second one
// must be refused by the tenant envelope.
func TestTenantEnvelopeBoundsEveryProject(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth: AuthConfig{Required: true, Keys: []string{
			"acme/research/one",
			"acme/batch/two",
		}},
		RateLimits: LimitConfig{
			RPM:      100,
			Tenants:  []string{"acme|rpm=2"},
			Projects: []string{"acme/research|rpm=2", "acme/batch|rpm=2"},
		},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	for i := 1; i <= 2; i++ {
		if w := post(h, "acme/research/one", req); w.Code != http.StatusOK {
			t.Fatalf("research %d: status %d, want 200", i, w.Code)
		}
	}
	if w := post(h, "acme/research/one", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("research 3: %d, want 429 — the partition limit did not apply", w.Code)
	}
	// A different project, same tenant. Its own partition is empty, so only the
	// envelope can refuse this.
	if w := post(h, "acme/batch/two", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("batch: %d, want 429 — a second project escaped the tenant envelope", w.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("engine saw %d calls, want 2", got)
	}
}

// Keys of one project share a bucket: they are the same budget wearing
// different credentials. One tenant must not be able to drain another's
// allowance, and rotating a key must not hand the caller a fresh one.
func TestKeysOfOneScopeShareABucket(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth: AuthConfig{Required: true, Keys: []string{
			"acme/research/one", "acme/research/two", "globex/research/three",
		}},
		RateLimits: LimitConfig{
			RPM:      100,
			Tenants:  []string{"acme|rpm=1", "globex|rpm=1000"},
			Projects: []string{"acme/research|rpm=1"},
		},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	if w := post(h, "acme/research/one", req); w.Code != http.StatusOK {
		t.Fatalf("acme/research/one: %d", w.Code)
	}
	// A different key, same scope: it shares the bucket, so it is refused.
	if w := post(h, "acme/research/two", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("acme/research/two: %d, want 429 — a second key got a fresh allowance", w.Code)
	}
	// A different tenant is unaffected.
	if w := post(h, "globex/research/three", req); w.Code != http.StatusOK {
		t.Errorf("globex/research/three: %d, want 200", w.Code)
	}
}

// A request that cannot be routed costs the tenant nothing. If a 404 leaked the
// reservation, a tenant pointing at a model name that does not exist would burn
// its whole minute's allowance on a typo.
func TestUnroutableRequestIsSettledForFree(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:       AuthConfig{Required: true, Keys: []string{"acme/research/admin"}},
		RateLimits: LimitConfig{Tenants: []string{"acme|rpm=1"}},
		Upstreams:  []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	for i := 0; i < 5; i++ {
		w := post(h, "acme/research/admin", `{"model":"does-not-exist","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("request %d: status %d, want 404", i, w.Code)
		}
	}
	if w := post(h, "acme/research/admin", req); w.Code != http.StatusOK {
		t.Errorf("after 5 unroutable requests: %d, want 200", w.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("engine saw %d calls, want 1", got)
	}
}

// A tenant nobody declared runs under the server defaults, which is what makes
// a bare configuration with one RPM value a working deployment.
func TestServerDefaultsApplyToUndeclaredTenants(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:       AuthConfig{Required: true, Keys: []string{"acme/research/plain"}},
		RateLimits: LimitConfig{RPM: 1},
		Upstreams:  []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})
	if w := post(h, "acme/research/plain", req); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	if w := post(h, "acme/research/plain", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("second: %d, want 429 — the server default was not applied", w.Code)
	}
}
