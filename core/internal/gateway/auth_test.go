package gateway

import (
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
	h, _, err := Build(cfg, nil, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
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
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/research/admin"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	if w := post(h, "acme/research/admin", req); w.Code != http.StatusOK {
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

// Two tenants may both name a key "admin". Storing credentials under the bare
// key id would make the second registration overwrite the first, and the first
// tenant's credential would then resolve to the second tenant's principal — one
// tenant reading and spending another's ledger. The credential is the whole
// tenant/project/keyid for exactly this reason.
func TestTwoTenantsMayNameAKeyTheSame(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := gatewayFor(t, Config{
		Auth: AuthConfig{Required: true, Keys: []string{
			"acme/research/admin",
			"globex/research/admin",
		}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: up.URL}},
	})

	for _, key := range []string{"acme/research/admin", "globex/research/admin"} {
		if w := post(h, key, req); w.Code != http.StatusOK {
			t.Errorf("%s: status %d, body %s", key, w.Code, w.Body)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("engine saw %d calls, want 2 — one key silently displaced the other", got)
	}
	// The bare key id belongs to nobody and must not resolve.
	if w := post(h, "admin", req); w.Code != http.StatusUnauthorized {
		t.Errorf("bare key id: status %d, want 401", w.Code)
	}
}

// Health must stay reachable without a key. A readiness probe cannot hold a
// credential, and a gateway whose /healthz returns 401 looks dead to Kubernetes
// while it is serving fine.
func TestHealthStaysOpenWhileChatIsGated(t *testing.T) {
	h := gatewayFor(t, Config{
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/research/admin"}},
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
