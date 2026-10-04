package apiserver

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Who may call the control plane.
//
// This is not the tenant key store and must not be confused with it. A tenant
// key spends money; it does not get to set the price, mint other keys, or close
// an invoice. An operator does, and an operator is one person or a handful of
// people who already have shell access — so the audience here is small by
// construction, and a shared token is the honest shape for it rather than a
// role table pretending otherwise. Per-role permissions are an enterprise
// capability precisely because they matter at a scale this does not have.
//
// One admin token, checked in constant time, with no route-level exemptions
// beyond the probes.

// requireAdmin rejects a request that does not carry the admin token.
//
// Empty token means the caller asserted nothing and is accepted, which is only
// safe because Config refuses to build a non-loopback server without one. That
// pairing is the whole safety argument, so the two are asserted together in
// NewServer rather than left to a comment.
func requireAdmin(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token != "" && !adminPresented(r, token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="fleet-control-plane"`)
				writeError(w, errs.Unauthenticated(
					"the control plane needs its admin token; set it on both sides"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// adminPresented reports whether the request carries the token.
//
// Constant time so a caller cannot learn the token one byte at a time by
// timing how long the comparison took. That matters more than usual here: the
// token is a shared operator credential, and unlike a password it is expected
// to travel over a plain link on a private network, where an oracle is cheap.
func adminPresented(r *http.Request, token string) bool {
	got := BearerToken(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// BearerToken reads the credential from the three places a client may put one.
//
// The same three the gateway accepts for a tenant key, so an operator can use
// curl, the console, or the gateway with one credential and one habit.
func BearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		// A bare Authorization value, which is what some HTTP clients send.
		if !strings.Contains(v, " ") {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// listensOffHost reports whether an address is reachable from anywhere but this
// machine.
//
// A wildcard host — empty, "0.0.0.0", "::" — is the case that matters, because
// it looks like a local choice in a unit file and is the opposite. An explicit
// loopback or a literal address is treated as deliberate.
func listensOffHost(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port at all is not something this server can bind; treat it as
		// unknown rather than as safe.
		return true
	}
	if _, err := strconv.Atoi(port); err != nil {
		return true
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	// A hostname: not resolvable here, and a name can resolve to anything.
	return !strings.EqualFold(host, "localhost")
}
