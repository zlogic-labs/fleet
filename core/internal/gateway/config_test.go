package gateway

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// A misconfiguration that only shows up as a runtime failure is a production
// outage. Each of these must be a startup error naming the fix, because the
// alternative is a gateway that is up, serving, and refusing everyone.
func TestStartupRejectsAuthMisconfiguration(t *testing.T) {
	cases := []struct {
		name string
		auth AuthConfig
		want string
	}{
		{
			// Serves nothing, and looks like an outage rather than a mistake.
			name: "required with no keys",
			auth: AuthConfig{Required: true},
			want: "set auth.keys or database.url",
		},
		{
			// The shape of a security setting someone believes is on.
			name: "keys configured but not required",
			auth: AuthConfig{Keys: []string{"acme/research/admin"}},
			want: "would be ignored",
		},
		{
			name: "a malformed key spec",
			auth: AuthConfig{Required: true, Keys: []string{"no-slash"}},
			want: "no-slash",
		},
		{
			name: "a key spec with no project",
			auth: AuthConfig{Required: true, Keys: []string{"acme/admin"}},
			want: "tenant/project/keyid",
		},
		{
			// An old configuration. Rejecting it is the point: an operator who
			// still believes rpm=10 is in force is worse off than one told it
			// moved.
			name: "a key spec still carrying limits",
			auth: AuthConfig{Required: true, Keys: []string{"acme/research/admin|rpm=10"}},
			want: "rate_limits.tenants",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Build(Config{
				Listen: "127.0.0.1:0", MaxBodyMB: 1, Auth: tc.auth,
			}, nil, detail.Nop{}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
			if err == nil {
				t.Fatal("startup succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The opposite failure: turning authentication on with valid keys must work, so
// that the checks above cannot be satisfied by rejecting everything.
func TestValidAuthConfigStarts(t *testing.T) {
	_, _, err := Build(Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/research/admin"}},
		RateLimits: LimitConfig{
			RPM:      600,
			Tenants:  []string{"acme|rpm=600,tpm=200000"},
			Projects: []string{"acme/research|rpm=60,tpm=20000"},
		},
	}, nil, detail.Nop{}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("a valid auth config was rejected: %v", err)
	}
}

// A database is a source of keys, so "required with an empty key list" is only
// an error when there is nowhere to look one up. Without this the obvious
// configuration — point Fleet at Postgres and let tenants live there — would be
// rejected by the very check that exists to catch a typo.
func TestDatabaseSatisfiesTheKeyRequirement(t *testing.T) {
	cfg := Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true},
		// No Build call: it would try to connect. This is validate alone,
		// which is what Load calls and what the check lives in.
	}
	if err := cfg.validate(nil); err == nil {
		t.Error("required with no keys and no database was accepted")
	}
	cfg.Database.URL = "postgres://localhost/fleet"
	if err := cfg.validate(nil); err != nil {
		t.Errorf("required with a database and no key list was rejected: %v", err)
	}
}

func TestAuthAndLimitEnvIsRead(t *testing.T) {
	t.Setenv("FLEET_AUTH_REQUIRED", "true")
	t.Setenv("FLEET_API_KEYS", "acme/research/admin;other/batch/b")
	t.Setenv("FLEET_RATE_TENANTS", "acme|rpm=600,tpm=200000")
	t.Setenv("FLEET_RATE_PROJECTS", "acme/research|rpm=60,tpm=20000")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.Required {
		t.Error("FLEET_AUTH_REQUIRED=true was not read")
	}
	if len(cfg.Auth.Keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(cfg.Auth.Keys))
	}
	if cfg.Auth.Keys[0] != "acme/research/admin" {
		t.Errorf("key[0] = %q — the semicolon split mangled it", cfg.Auth.Keys[0])
	}
	if len(cfg.RateLimits.Tenants) != 1 || len(cfg.RateLimits.Projects) != 1 {
		t.Fatalf("limit lists = %+v, want one tenant and one project", cfg.RateLimits)
	}
	// The env path has to resolve the same tables the file path does, or a
	// containerised deployment is limited by something other than what it was
	// configured with.
	p := limiterMust(t, cfg).PolicyFor(ratelimit.Project("acme", "research"))
	if p.Envelope.RequestsPerMinute != 600 || p.Partition.RequestsPerMinute != 60 {
		t.Errorf("policies = %+v / %+v, want 600 and 60", p.Envelope, p.Partition)
	}
}
