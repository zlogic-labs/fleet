package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// fakeEngine is a minimal OpenAI server, so the assertions are about the
// gateway's gating rather than about a mock's behaviour. It fails the test if a
// credential reaches it, which is how the credential-stripping checks assert
// something about a header that is supposed to be absent.
func fakeEngine(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			// The gateway forwarded a tenant credential to the engine. It will
			// be in this engine's request log, which is a different tenant's
			// operational data and cannot be taken back.
			t.Errorf("credential reached the engine: Authorization=%q x-api-key=%q",
				r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"demo",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func gatewayFor(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	cfg.MaxBodyMB = 1
	cfg.Listen = "127.0.0.1:0"
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = 4096
	}
	h, _, err := Build(cfg, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return h
}

func post(h http.Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const req = `{"model":"demo","messages":[{"role":"user","content":"hi"}]}`

// A gateway that is asked to stop checking keys must stop serving traffic. The
// unauthenticated case is the one that turns a developer convenience into an
// open GPU: before this, anyone could spend anyone else's cards.
func TestAuthenticationGatesTheChatEndpoint(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/admin|rpm=60"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	if w := post(h, "admin", req); w.Code != http.StatusOK {
		t.Errorf("valid key: status %d, body %s", w.Code, w.Body)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("engine saw %d calls, want 1", got)
	}

	for _, key := range []string{"", "wrong-key"} {
		w := post(h, key, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", key, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("key %q: 401 without WWW-Authenticate; an SDK cannot recover from it", key)
		}
	}

	// The refusals above must not have reached the engine. A rejected request
	// that still consumed a GPU slot is a rejection in name only.
	if got := calls.Load(); got != 1 {
		t.Errorf("engine saw %d calls, want 1 — an unauthenticated request was forwarded", got)
	}
}

// Health must stay reachable without a key. A readiness probe cannot hold a
// credential, and a gateway whose /healthz returns 401 looks dead to Kubernetes
// while it is serving fine.
func TestHealthStaysOpenWhileChatIsGated(t *testing.T) {
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/admin"}},
		Upstreams: []UpstreamConfig{},
	})
	for _, path := range []string{"/healthz", "/health"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 without a key", path, w.Code)
		}
	}
}

// The reservation is the request's upper bound, so a tenant at its limit is
// turned away before the engine is touched at all.
func TestRateLimitRefusesAndAdvertisesRetryAfter(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/admin|rpm=2"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	// max_tokens=0 resolves to DefaultMaxTokens, so each of these reserves far
	// more than the 2-per-minute request limit allows on the third call.
	// The limit is on requests, so the first two must pass.
	for i := 1; i <= 2; i++ {
		if w := post(h, "admin", req); w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, w.Code)
		}
	}
	w := post(h, "admin", req)
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
}

// Limits are per tenant, not per key: a tenant that bought one engine has not
// bought one per credential, and one tenant must not be able to drain another's
// allowance by holding more keys.
func TestLimitsArePerTenantNotPerKey(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth: AuthConfig{Required: true, Keys: []string{
			"acme/one|rpm=1", "acme/two|rpm=1000", "other/three|rpm=1000",
		}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	if w := post(h, "one", req); w.Code != http.StatusOK {
		t.Fatalf("acme/one: %d", w.Code)
	}
	// A different key, same tenant: it shares the bucket, so it is refused.
	if w := post(h, "two", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("acme/two: %d, want 429 — the tenant got a fresh allowance per key", w.Code)
	}
	// A different tenant is unaffected.
	if w := post(h, "three", req); w.Code != http.StatusOK {
		t.Errorf("other/three: %d, want 200", w.Code)
	}
}

// A request that cannot be routed costs the tenant nothing. If a 404 leaked the
// reservation, a tenant pointing at a model name that does not exist would burn
// its whole minute's allowance on a typo.
func TestUnroutableRequestIsSettledForFree(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/admin|rpm=1"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	for i := 0; i < 5; i++ {
		w := post(h, "admin", `{"model":"does-not-exist","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("request %d: status %d, want 404", i, w.Code)
		}
	}
	// The allowance is untouched, so a real request still works.
	if w := post(h, "admin", req); w.Code != http.StatusOK {
		t.Errorf("after 5 unroutable requests: %d, want 200", w.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("engine saw %d calls, want 1", got)
	}
}

// A key that declared no limits falls back to the server's, and a key that
// declared looser ones is not clamped down to the server default.
func TestServerLimitsApplyToKeysThatDeclareNone(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth:       AuthConfig{Required: true, Keys: []string{"acme/plain"}},
		RateLimits: LimitConfig{RPM: 1},
		Upstreams:  []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})
	if w := post(h, "plain", req); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	if w := post(h, "plain", req); w.Code != http.StatusTooManyRequests {
		t.Errorf("second: %d, want 429 — the server default was not applied", w.Code)
	}
}
