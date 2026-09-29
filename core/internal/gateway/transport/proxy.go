// Package transport proxies a request to an inference endpoint while observing
// the response. It is the only place in the gateway that writes bytes on the
// response path, which keeps streaming, flushing and usage collection in one
// reviewable unit.
package transport

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Authorize sets the credential the engine expects. It is a callback rather
// than a field on Endpoint for two reasons: the client's own API key must
// never reach a backend that may log request headers, and a tenant key is not
// the engine's key. Returning no error keeps the choice explicit — an engine
// that needs a credential and did not get one answers 401, which we forward.
type Authorize func(*http.Request, engine.Endpoint)

// Options configures a Proxy. The zero value is usable and means "use the
// default transport and inject no backend credential".
type Options struct {
	// Transport is the outbound round tripper. One is built if nil. It should
	// be tuned for long-lived, high-concurrency streaming: the gateway holds a
	// connection open for the entire generation time of a request.
	Transport http.RoundTripper
	Authorize Authorize
	// RetainCap bounds the response bytes kept for usage parsing.
	RetainCap int
	Clock     func() time.Time
}

type Proxy struct {
	rp        *httputil.ReverseProxy
	authorize Authorize
}

func New(opts Options) *Proxy {
	p := &Proxy{authorize: opts.Authorize}
	p.rp = &httputil.ReverseProxy{
		Transport:      opts.Transport,
		Rewrite:        p.rewrite,
		ModifyResponse: p.observeResponse,
		ErrorHandler:   p.writeTransportError,
		// A negative interval flushes every write immediately. Any other
		// value batches writes, which turns a streamed completion into a
		// stall of up to the interval and defeats the point of streaming.
		FlushInterval: -1,
	}
	return p
}

// Serve proxies r to ep. A nil tap is allowed and simply skips accounting.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, ep engine.Endpoint, tap *Tap) {
	ctx := withEndpoint(r.Context(), ep)
	if tap != nil {
		ctx = withTap(ctx, tap)
	}
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// rewrite retargets the outbound request. The inbound request is never
// mutated: httputil hands us a clone, and the gateway still needs the
// original afterwards to settle billing.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	ep, _ := endpointFrom(pr.In.Context())

	if target, err := url.Parse(trimJoin(ep.BaseURL, pr.In.URL.RequestURI())); err == nil {
		pr.SetURL(target)
		pr.Out.Host = target.Host
	}

	// Never forward a client Accept-Encoding. A compressed body breaks frame
	// scanning and defeats incremental flushing, and a usage chunk we cannot
	// read in the clear is a usage chunk we cannot bill.
	pr.Out.Header.Set("Accept-Encoding", "identity")

	if p.authorize != nil {
		p.authorize(pr.Out, ep)
	}
}

// observeResponse attaches the tap to a successful body. A 4xx is left
// untouched: it is the caller's own error and the engine describes it better
// than we can, so rewriting it would only lose detail.
func (p *Proxy) observeResponse(resp *http.Response) error {
	if resp.StatusCode >= 500 {
		return rewriteServerError(resp)
	}
	tap, _ := resp.Request.Context().Value(tapKey{}).(*Tap)
	if tap == nil {
		return nil
	}
	// The engine, not the client request, decides whether the body is a
	// stream: a client can ask for stream=true and still receive JSON.
	tap.UseSSE(isEventStream(resp.Header.Get("Content-Type")))
	resp.Body = tap.Reader(resp.Body)
	return nil
}

// writeTransportError handles a failure to reach the engine at all: DNS, a
// refused connection, a dial timeout. Without this the client would get Go's
// plain-text 502 page and every SDK would fail to parse it.
//
// The status is 503 rather than 502 because from the caller's side the model
// is unavailable, not the request malformed — which is the signal that
// permits a retry against another replica.
func (p *Proxy) writeTransportError(w http.ResponseWriter, _ *http.Request, err error) {
	openai.WriteError(w, errs.Unavailable("cannot reach the inference endpoint: %s", err))
}

func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// trimJoin concatenates a base URL and a request URI without doubling or
// dropping the separator. Endpoint bases come from Kubernetes Services, so the
// trailing slash is not ours to control.
func trimJoin(base, requestURI string) string {
	if base == "" {
		return requestURI
	}
	return strings.TrimRight(base, "/") + requestURI
}

// endpointKey and tapKey are distinct types so that the two context values
// cannot collide with anything a caller stores.
type endpointKey struct{}
type tapKey struct{}

func withEndpoint(ctx context.Context, ep engine.Endpoint) context.Context {
	return context.WithValue(ctx, endpointKey{}, ep)
}

func endpointFrom(ctx context.Context) (engine.Endpoint, bool) {
	ep, ok := ctx.Value(endpointKey{}).(engine.Endpoint)
	return ep, ok
}

func withTap(ctx context.Context, tap *Tap) context.Context {
	return context.WithValue(ctx, tapKey{}, tap)
}
