package apiserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/registry"
)

// The control plane holds the price book, the tenant table and the key issuer.
// Before this middleware it had no authentication at all, which was verified
// against a running deployment: an unauthenticated POST created a tenant, a
// second created a project, a third returned a working `sk-fleet-…` secret in
// clear text, and that secret then ran inference on the gateway. The same
// unauthenticated POST rewrote the GPU-hour rate every tenant is billed.
//
// So these tests are written as the exploit, not as a checklist.

const probeToken = "operator-secret"

func serverWith(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.Blobs == nil {
		dir := t.TempDir()
		cfg.Blobs, _ = blobstore.NewFS(dir)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewServer(cfg, registry.NewMemory(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); s.Close() })
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestAManagementRouteRefusesACallerWithNoToken(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken}})

	// Every verb that changes money or issues a credential. If one of these
	// answers 200 the exploit still works with a token check bolted on
	// somewhere else.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tenants", ""},
		{"POST", "/api/v1/tenants", `{"id":"x","name":"X"}`},
		{"POST", "/api/v1/projects", `{"tenantId":"x","name":"p"}`},
		{"POST", "/api/v1/keys", `{"projectId":"x/p","name":"k"}`},
		{"POST", "/api/v1/cost-rates", `{"cluster":"c","gpuHourMicro":1}`},
		{"PUT", "/api/v1/cost-periods/1999-01", ""},
		{"PUT", "/api/v1/inventory", `{"cluster":{"name":"c"}}`},
		{"GET", "/api/v1/spend", ""},
	} {
		code, _ := call(t, srv, c.method, c.path, "", c.body)
		if code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s %s returned %d, want 401", c.method, c.path, code)
		}
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken}})
	for _, bad := range []string{"", probeToken + "x", probeToken[:len(probeToken)-1], "Bearer " + probeToken} {
		code, _ := call(t, srv, "GET", "/api/v1/tenants", bad, "")
		if code != http.StatusUnauthorized {
			t.Errorf("token %q returned %d, want 401", bad, code)
		}
	}
}

// A token is accepted wherever it arrives: the header, the x-api-key spelling,
// and a bare Authorization value with no scheme. An operator switching between
// curl and the console should not have to learn two conventions.
func TestTheRightTokenIsAccepted(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken}})

	// /models rather than /tenants: without a database the tenancy routes
	// answer 400, and a test that cannot tell 400 from 401 tests nothing.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+probeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the admin token was refused with %d", resp.StatusCode)
	}

	for _, header := range []struct{ name, value string }{
		{"X-Api-Key", probeToken},
		{"Authorization", probeToken},
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/models", nil)
		req.Header.Set(header.name, header.value)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get with %s: %v", header.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s carried the token but returned %d", header.name, resp.StatusCode)
		}
	}
}

// The probes stay outside the guard. A kubelet has no way to carry a bearer
// token, so a liveness check behind one reports a healthy server as dead — the
// gateway already learned that lesson and it applies here unchanged.
func TestTheProbesStayOpen(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken}})
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := call(t, srv, "GET", p, "", ""); code != http.StatusOK {
			t.Errorf("%s returned %d, want 200 — a probe cannot carry a token", p, code)
		}
	}
}

func TestAnUnauthorizedAnswerIsAnOpenAIStyleEnvelope(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken}})
	resp, err := http.Get(srv.URL + "/api/v1/tenants")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate is %q; a client cannot tell what to send", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code == "" || !strings.Contains(body.Error.Message, "admin token") {
		t.Errorf("error = %+v; it must say which credential is missing", body.Error)
	}
}

// A loopback control plane is a supported configuration — the gateway on the
// same machine is the common case — so an absent token must not stop it.
func TestALoopbackServerNeedsNoToken(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081"} {
		srv := serverWith(t, Config{Listen: addr})
		if code, _ := call(t, srv, "GET", "/api/v1/models", "", ""); code != http.StatusOK {
			t.Errorf("loopback %s with no token returned %d, want 200", addr, code)
		}
	}
}

// The wildcard is the case that matters, because it looks like a local choice
// in a unit file and is the exact opposite.
func TestAWildcardBindRefusesToStartWithoutAToken(t *testing.T) {
	for _, addr := range []string{"", ":8081", "0.0.0.0:8081", "[::]:8081", "10.0.0.5:8081"} {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		blobs, _ := blobstore.NewFS(t.TempDir())
		_, err := NewServer(Config{Listen: addr, Blobs: blobs}, registry.NewMemory(), log)
		if err == nil {
			t.Errorf("%q started with no admin token; it serves keys and prices", addr)
		}
	}
}
