package apiserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// ── plumbing ───────────────────────────────────────────────────

var errNoStore = &noStoreError{}

type noStoreError struct{}

func (*noStoreError) Error() string {
	return "no object store configured; the control plane has nowhere to put weights"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			attrs := []any{
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", strconv.FormatInt(rec.bytes, 10),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			// A 5xx with only a status is unactionable: the response says
			// "internal error" precisely because the cause is not safe to
			// hand the caller, which means it has to appear here or nowhere.
			if rec.err != nil {
				attrs = append(attrs, "error", rec.err)
			}
			log.Log(r.Context(), level, "request", attrs...)
		})
	}
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "path", r.URL.Path, "panic", v)
					openai.WriteError(w, errs.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	// err is the cause behind a 5xx, kept so the log can name it.
	err error
}

// Fail records the cause of a server-side failure. Handlers call it before
// answering; the response body still says nothing about it.
func (r *recorder) Fail(err error) { r.err = err }

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
