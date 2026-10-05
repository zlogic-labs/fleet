// Package httpx holds the HTTP plumbing both Fleet servers need and neither
// should own.
//
// The gateway and the control plane had their own recoverer, request logger
// and status recorder, and the two copies had already diverged: the control
// plane's recorder counted bytes and could name the cause behind a 5xx, while
// the gateway's tracked whether a header had gone out. That flag matters --
// without it a handler that writes 200 and then 500 has its log line say 500
// while the client received 200, which is the kind of disagreement that makes
// a log useless for the one thing logs are for.
//
// One copy, with both behaviours, so neither server drifts from the other
// again.
package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

type ctxKey struct{}

var statusKey ctxKey

// Recorder observes a response on its way out.
type Recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	// err is the cause behind a 5xx, kept so the log can name it.
	err error
	// written records that a status has gone out. Without it the first status
	// is not the status, and a handler that answers 200 then fails would be
	// logged as the failure the client never saw.
	written bool
}

// NewRecorder wraps w with a recorder that assumes 200 until told otherwise.
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, status: http.StatusOK}
}

// Fail records the cause of a server-side failure. Handlers call it before
// answering; the response body still says nothing about it.
func (r *Recorder) Fail(err error) { r.err = err }

// Status is the status the client actually received.
func (r *Recorder) Status() int { return r.status }

func (r *Recorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer. Without it, wrapping the writer
// silently disables the incremental flushing that streaming depends on -- and
// the symptom would be a completion that arrives all at once at the end.
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WithRecorder publishes rec on the request context, so middleware further out
// -- the recoverer, most importantly -- can see what was already sent.
func WithRecorder(r *http.Request, rec *Recorder) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), statusKey, rec))
}

// RecorderFrom returns the recorder on the request, if RequestLog ran.
func RecorderFrom(r *http.Request) (*Recorder, bool) {
	rec, ok := r.Context().Value(statusKey).(*Recorder)
	return rec, ok
}

// RecorderOf returns the recorder wrapping w, if w is one.
//
// For a handler that has only the writer -- which is every handler whose job
// is to answer an error -- and so cannot reach the request context.
func RecorderOf(w http.ResponseWriter) (*Recorder, bool) {
	rec, ok := w.(*Recorder)
	return rec, ok
}

// HeaderWritten reports whether a status has already gone out, which is the
// signal that a response can no longer be replaced.
func HeaderWritten(r *http.Request) bool {
	rec, ok := RecorderFrom(r)
	return ok && rec.written
}

// RequestLog records method, path, status, size and duration.
//
// It deliberately omits the body and the Authorization header: this is an
// inference gateway, so a request log is a prompt log.
func RequestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := NewRecorder(w)
			next.ServeHTTP(rec, WithRecorder(r, rec))

			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			attrs := []any{
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "bytes", strconv.FormatInt(rec.bytes, 10),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			// A 5xx with only a status is unactionable: the response says
			// "internal error" precisely because the cause is not safe to hand
			// the caller, which means it has to appear here or nowhere.
			if rec.err != nil {
				attrs = append(attrs, "error", rec.err)
			}
			log.Log(r.Context(), level, "request", attrs...)
		})
	}
}

// Recoverer turns a panic into an OpenAI error envelope and a log line.
//
// ErrAbortHandler is re-panicked rather than reported: it is how net/http says
// "the client went away", and answering it would mean writing to a dead socket
// and logging a routine disconnect as a server fault.
//
// A response that already has a status is left alone. A streamed completion can
// have committed a 200 and several SSE frames before something downstream
// panics, and writing an error envelope at that point appends a JSON object to
// a stream of frames -- which every SDK parses as a malformed delta. The panic
// is still logged; there is simply nothing left to tell the client.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic", "path", r.URL.Path, "panic", v,
					"stack", string(debug.Stack()))
				if !HeaderWritten(r) {
					openai.WriteError(w, errs.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
