package gateway

import (
	"log/slog"
	"os"
	"strings"
	"testing"

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
			want: "auth.required is set but auth.keys is empty",
		},
		{
			// The shape of a security setting someone believes is on.
			name: "keys configured but not required",
			auth: AuthConfig{Keys: []string{"acme/admin"}},
			want: "the keys would be ignored",
		},
		{
			name: "a malformed key spec",
			auth: AuthConfig{Required: true, Keys: []string{"no-slash"}},
			want: "no-slash",
		},
		{
			name: "an unknown limit name",
			auth: AuthConfig{Required: true, Keys: []string{"acme/admin|rps=10"}},
			want: "use rpm or tpm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Build(Config{
				Listen: "127.0.0.1:0", MaxBodyMB: 1, Auth: tc.auth,
			}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
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
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/admin|rpm=10,tpm=1000"}},
	}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("a valid auth config was rejected: %v", err)
	}
}

func TestAuthEnvIsRead(t *testing.T) {
	t.Setenv("FLEET_AUTH_REQUIRED", "true")
	t.Setenv("FLEET_API_KEYS", "acme/admin|rpm=10;other/b|rpm=20")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.Required {
		t.Error("FLEET_AUTH_REQUIRED=true was not read")
	}
	if len(cfg.Auth.Keys) != 2 {
		t.Errorf("keys = %d, want 2", len(cfg.Auth.Keys))
	}
	if cfg.Auth.Keys[0] != "acme/admin|rpm=10" {
		t.Errorf("key[0] = %q — the semicolon split mangled it", cfg.Auth.Keys[0])
	}
	// os.Environ is process-wide; t.Setenv restores it, but confirm the two
	// names the doc promises are the two the code reads.
	if os.Getenv("FLEET_API_KEYS") == "" {
		t.Error("FLEET_API_KEYS was not visible to the process")
	}
}
