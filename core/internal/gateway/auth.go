package gateway

import (
	"net/http"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
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

// There is deliberately no capability gate here yet, and that is a decision
// rather than an omission.
//
// A gate with no gated feature is a gate nobody tests: the route list would say
// which endpoints check it, the tests would say the check works, and the first
// real feature would find out at runtime. So the entitlement boundary is
// License.Granted -- which /fleet/status already answers from, and which no
// handler consults -- and the middleware appears when there is a route to put
// it on. The claim the console makes, "gated in the gateway, not hidden in the
// browser", is true of the credential and of nothing else yet.

func unauthorized(w http.ResponseWriter, err error) {
	// RFC 6750 says a 401 must advertise how to authenticate. An SDK that
	// reads the header retries with a key; one that only reads the body gives
	// up, and telling it what header it needed is the difference between a
	// recoverable error and a support ticket.
	w.Header().Set("WWW-Authenticate", `Bearer realm="fleet"`)
	openai.WriteError(w, err)
}
