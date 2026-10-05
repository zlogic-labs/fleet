package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// Serving: the listener, the drain, and the two timeouts.
//
// Split from Run because Run is wiring — a database, a sink, a registry, a
// refresher, a pruner — and none of that is about accepting connections. What
// is left here is the part where the process actually serves, and it is short
// enough to read in one go.

// shutdownGrace bounds the drain after a signal.
//
// Long enough for a streamed completion to finish writing its last frame, and
// bounded so a wedged one cannot hold the process open indefinitely. A pod that
// will not exit gets killed instead, which loses a stream rather than a
// shutdown.
const shutdownGrace = 30 * time.Second

// serve blocks until ctx is cancelled, then drains.
//
// The listener is opened before anything is announced, so a port that is already
// taken is a start-up error and not a log line followed by a process that looks
// alive.
func serve(ctx context.Context, cfg Config, handler http.Handler, lic entitlement.License, log *slog.Logger, version string) error {
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// No WriteTimeout: a streamed completion can legitimately run for
		// minutes, and a write deadline would cut it off mid-answer. The
		// per-request context is what bounds a client that has gone away.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Every in-flight request derives from ctx, so a signal cancels
		// generation rather than leaving streams hanging until the deadline.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("fleet-gateway listening",
		"addr", cfg.Listen, "edition", lic.Edition, "version", version,
		"upstreams", len(cfg.Upstreams), "ui", "http://"+cfg.Listen+"/")

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Detached from ctx on purpose: ctx is already cancelled, which is the
	// signal being acted on, and a drain built on it would abort immediately.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("fleet-gateway stopped")
	return nil
}
