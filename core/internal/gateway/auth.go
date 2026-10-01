package gateway

import (
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// authenticate resolves the caller and puts them on the context.
//
// It runs before the body is read on purpose. A request with no valid key costs
// nothing to reject, and rejecting it after tokenising the prompt would mean an
// unauthenticated caller can make the gateway do work — which is the shape of a
// free denial-of-service tool.
func authenticate(auth *authn.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := auth.Authenticate(r.Context(), r)
			if err != nil {
				unauthorized(w, err)
				return
			}
			// Stripped here rather than at the proxy: by the time a handler
			// forgets, the credential has already been forwarded to an engine
			// that logs request headers, and a tenant key in another tenant's
			// logs is a credential leak that cannot be undone.
			authn.StripCredentials(r)
			next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
		})
	}
}

// requireCapability rejects a caller the licence does not cover.
//
// It is a separate middleware rather than a check inside a handler because the
// answer is the same for every route that uses it, and a handler that forgets
// to ask would expose a paid capability for free — the one failure an edition
// boundary exists to prevent.
func requireCapability(auth *authn.Authenticator, c entitlement.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := authn.FromContext(r.Context())
			if !p.Granted(c, time.Now(), auth.Lic) {
				openai.WriteError(w, errs.PermissionDenied(
					"this capability (%s) is not in your edition", c))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter, err error) {
	// RFC 6750 says a 401 must advertise how to authenticate. An SDK that
	// reads the header retries with a key; one that only reads the body gives
	// up, and telling it what header it needed is the difference between a
	// recoverable error and a support ticket.
	w.Header().Set("WWW-Authenticate", `Bearer realm="fleet"`)
	openai.WriteError(w, err)
}
