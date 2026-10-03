package gateway

import (
	"net/http"

	"github.com/zlogic-labs/fleet/core/pkg/metrics"
)

// GET /metrics.
//
// Behind authentication, unlike /healthz, and the difference is deliberate.
//
// Liveness must be reachable without a credential: a probe that needs one is a
// probe that takes the service down when a key expires. This endpoint is not
// liveness. It carries per-tenant token counts and spend, which is billing
// data, and billing data is what authentication exists to protect. A
// Prometheus scraper carries a bearer token -- that is a solved problem and a
// poor reason to publish cost figures unauthenticated.
//
// It shares the API's middleware, so it also inherits the refusal accounting: a
// scrape with a bad key appears in fleet_refused_total rather than looking like
// a fleet that has simply gone quiet.
func serveMetrics(reg *metrics.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// The exposition content type carries the format version, and a client
		// that negotiates it parses strictly. This writes 0.0.4 text.
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if _, err := reg.WriteTo(w); err != nil {
			// The status line is already committed, so there is nothing left
			// to tell this client. A truncated exposition is discarded by the
			// server as invalid rather than half-imported, which is the right
			// outcome; the failure reaches the operator through the gateway's
			// own request log.
			http.Error(w, "", http.StatusInternalServerError)
		}
	}
}
