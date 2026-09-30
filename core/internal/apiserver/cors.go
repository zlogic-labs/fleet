package apiserver

import (
	"net/http"
	"strconv"
	"strings"
)

// cors lets the console, served from the gateway's origin, call the control
// plane on its own port.
//
// This is a two-process design and it stays that way: the gateway is on the
// request path and must not be able to fail because the control plane is
// restarting. The cost is that the browser sees two origins, so the control
// plane has to answer preflights.
//
// The default is deliberately narrow: any loopback port, so the console can
// move between 8080, 5173 and whatever else during development, and nothing
// else. A public deployment sets FLEET_ALLOWED_ORIGINS to the one origin that
// serves the console, and a browser on another site then cannot read the
// registry.
func cors(allowed []string) func(http.Handler) http.Handler {
	allowAll := len(allowed) == 0

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowAll && isLoopback(origin) || contains(allowed, origin)) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// The origin varies per request, so a cached response must not
				// be replayed to a different page.
				w.Header().Add("Vary", "Origin")
				// The console sends Authorization for the tenant's key, and
				// without naming it here the preflight fails and every
				// management call is blocked.
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				// Answering the preflight with 204 without a matching
				// Allow-Origin is what makes the browser report a CORS error
				// rather than a 404, so the status stays 204 either way.
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopback reports whether an origin points at this machine.
//
// The host is parsed rather than substring-matched: "http://evil-localhost.com"
// contains "localhost" and is not localhost.
func isLoopback(origin string) bool {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
		if !ok {
			return false
		}
	}
	authority := rest
	if slash := strings.IndexByte(authority, '/'); slash >= 0 {
		authority = authority[:slash]
	}
	host := authority
	if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		// Reject a non-numeric port, so "localhost:evil" does not pass as a
		// bare host.
		if _, err := strconv.Atoi(authority[colon+1:]); err != nil {
			return false
		}
		host = authority[:colon]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// CorsOrigins parses a comma-separated allowlist.
func CorsOrigins(env string) []string {
	raw := strings.TrimSpace(env)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, strings.TrimRight(v, "/"))
		}
	}
	return out
}
