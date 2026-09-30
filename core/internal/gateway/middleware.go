package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
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

// recoverer turns a panic into a 500 with an OpenAI error envelope, because
// net/http's default handler writes a plain-text body that no SDK can parse.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// ReverseProxy panics with ErrAbortHandler when the client
				// disconnects mid-stream. That is its normal way of stopping
				// work, and net/http recovers it silently on purpose. Turning
				// it into a 500 would log a stack trace for every cancelled
				// request and then try to write a body to a dead socket.
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic serving request",
					"path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				// A stream may have already committed a 200, in which case
				// there is no envelope left to write.
				if !headerWritten(r) {
					openai.WriteError(w, errs.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestLog records method, path, status and duration. It deliberately omits
// the body and the Authorization header: this is an inference gateway, so a
// request log is a prompt log.
func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			// The recoverer runs outside this middleware and cannot see the
			// recorder, so it is published on the request context.
			r = r.WithContext(context.WithValue(r.Context(), statusKey, rec))

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		})
	}
}

// headerWritten reports whether a status has already gone out, which is the
// signal that a response can no longer be replaced.
func headerWritten(r *http.Request) bool {
	if rec, ok := r.Context().Value(statusKey).(*statusRecorder); ok {
		return rec.written
	}
	return false
}

type ctxKey struct{}

var statusKey ctxKey

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer. Without it, wrapping the writer
// would silently disable the incremental flushing that streaming depends on —
// and the symptom would be a completion that arrives all at once at the end.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
