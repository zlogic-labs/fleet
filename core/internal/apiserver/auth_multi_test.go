package apiserver

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/registry"
)

const secondProbeToken = "operator-secret-2"

// Two operators, two tokens. The alternative is a shared credential, where one
// person leaving forces a rotation that invalidates whatever the others are
// using at the time.

func TestEveryAdminTokenWorks(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{probeToken, secondProbeToken}})
	for _, tok := range []string{probeToken, secondProbeToken} {
		if code, _ := call(t, srv, "GET", "/api/v1/models", tok, ""); code != http.StatusOK {
			t.Errorf("token %q returned %d, want 200", tok, code)
		}
	}
}

// Revoking is a configuration change and a restart, not a runtime mutation, so
// this is the honest shape of the property: the server the operator restarts
// with must refuse the token they removed while serving the one they kept.
func TestARevokedTokenIsRefusedByTheNextStart(t *testing.T) {
	before := serverWith(t, Config{AdminTokens: []string{probeToken, secondProbeToken}})
	if code, _ := call(t, before, "GET", "/api/v1/models", probeToken, ""); code != http.StatusOK {
		t.Fatalf("before revoking, the token returned %d", code)
	}
	before.Close()

	after := serverWith(t, Config{AdminTokens: []string{secondProbeToken}})
	if code, _ := call(t, after, "GET", "/api/v1/models", probeToken, ""); code != http.StatusUnauthorized {
		t.Fatalf("after revoking, it returned %d, want 401", code)
	}
	if code, _ := call(t, after, "GET", "/api/v1/models", secondProbeToken, ""); code != http.StatusOK {
		t.Fatalf("the surviving token returned %d, want 200", code)
	}
}

func TestATokenIsComparedAgainstEveryEntry(t *testing.T) {
	// This checks that the *last* configured token works, which is a real
	// property but NOT proof that the comparison does not short-circuit: a
	// short-circuiting guard returns the same answer here, and the difference is
	// only in elapsed time, which a unit test cannot observe. The accumulation in
	// adminPresented is there because returning early would leak which token
	// matched -- it is justified by that reasoning, not by a test, and it was
	// mutation-checked only to confirm the mutation is unobservable rather than
	// harmless.
	srv := serverWith(t, Config{AdminTokens: []string{probeToken, secondProbeToken}})
	if code, _ := call(t, srv, "GET", "/api/v1/models", secondProbeToken, ""); code != http.StatusOK {
		t.Fatalf("the last token returned %d, want 200", code)
	}
}

// Blanks do not shadow a real token. They are dropped, so a stray empty entry
// left over from a templated unit file cannot lock everyone out.
func TestABlankTokenAlongsideARealOneIsIgnored(t *testing.T) {
	srv := serverWith(t, Config{AdminTokens: []string{"", probeToken, "  "}})
	if code, _ := call(t, srv, "GET", "/api/v1/models", probeToken, ""); code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
}

// Blanks alone mean nothing is configured, and on a loopback address that is
// the documented pairing: no credential required. This is deliberately not a
// refusal, because the same empty configuration on a wildcard bind is refused
// at startup instead -- which is the next test.
func TestBlanksAloneCountAsNothingConfigured(t *testing.T) {
	srv := serverWith(t, Config{Listen: "127.0.0.1:0", AdminTokens: []string{"", "   "}})
	if code, _ := call(t, srv, "GET", "/api/v1/models", "", ""); code != http.StatusOK {
		t.Fatalf("got %d, want 200 on loopback with nothing configured", code)
	}
}

// The startup check and the guard must agree on what "configured" means. If the
// check counted a blank as configured, a wildcard bind would start and then
// refuse every request; if the guard counted one, the check would have to as
// well and a blank would look like protection on the command line.
func TestAWildcardBindWithOnlyBlankTokensIsRefused(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := NewServer(Config{Listen: "0.0.0.0:8081", AdminTokens: []string{"", "  "}}, registry.NewMemory(), log)
	if err == nil {
		t.Fatal("a wildcard bind with no usable token was allowed to start")
	}
}

func TestAdminTokensDropsBlanksAndTrims(t *testing.T) {
	got := adminTokens([]string{" a ", "", "b", "   ", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestAdminTokensOnAnEmptyInputIsEmpty(t *testing.T) {
	if len(adminTokens(nil)) != 0 || len(adminTokens([]string{})) != 0 {
		t.Fatal("an empty input produced tokens")
	}
}
