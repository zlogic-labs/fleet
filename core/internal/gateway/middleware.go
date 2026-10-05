package gateway

import (
	"net"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// outboundTransport builds the round tripper used to reach inference engines.
//
// The settings here are not defaults; each one is a response to a specific
// failure mode of a long-lived streaming proxy.
func outboundTransport(t TimeoutConfig) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		DialContext: (&net.Dialer{
			Timeout:   t.Dial,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		// A connection is held for the entire generation of a response, which
		// is far longer than a typical API's idle timeout. The default of 90s
		// would churn the pool under load and re-run TCP plus TLS handshakes
		// in the request path.
		MaxIdleConns:          2048,
		MaxIdleConnsPerHost:   512,
		IdleConnTimeout:       180 * time.Second,
		MaxConnsPerHost:       0,
		ResponseHeaderTimeout: t.ResponseHdrs,
		ExpectContinueTimeout: time.Second,

		WriteBufferSize: 32 << 10,
		ReadBufferSize:  32 << 10,

		ForceAttemptHTTP2: true,
	}
}

// upstreamAuthorizer injects each upstream's own credential.
//
// The client's API key is never forwarded: a tenant key presented to an engine
// that logs request headers leaks one tenant's credential into another's
// operational logs.
func upstreamAuthorizer(ups []UpstreamConfig) transport.Authorize {
	keys := make(map[string]string, len(ups))
	for _, up := range ups {
		keys[up.ID] = up.APIKey
	}
	if len(keys) == 0 {
		return nil
	}
	return func(r *http.Request, ep engine.Endpoint) {
		if key := keys[ep.ID]; key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
	}
}
