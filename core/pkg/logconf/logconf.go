// Package logconf builds the logger both binaries start with.
//
// It existed as two copies, in each cmd, and they had already diverged: the
// gateway wrote JSON and the control plane wrote text, from otherwise identical
// switches. A copy that has silently drifted is worse than either version --
// reading one tells you nothing certain about the other.
//
// The format is a parameter rather than a per-binary choice because the two
// formats are for different consumers and that difference is real: the gateway
// emits one line per request into whatever aggregates it, and wants JSON; the
// control plane is read by a human following a failed deployment, and wants
// something they can read without a decoder. Both are stated at the call site.
package logconf

import (
	"log/slog"
	"os"
)

// Format selects the handler.
type Format string

const (
	// JSON is one object per line, for machines.
	JSON Format = "json"
	// Text is for a human reading a terminal.
	Text Format = "text"
)

// New returns a logger at the named level.
//
// An unknown level is Info rather than an error: the level is a convenience
// switch on a command line, and refusing to start over a typo is a worse
// failure than logging one level too much.
func New(level string, format Format) *slog.Logger {
	return slog.New(newHandler(level, format, os.Stderr))
}

func newHandler(level string, format Format, w *os.File) slog.Handler {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if format == Text {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
