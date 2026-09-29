// Package errs provides typed errors carrying enough information for a
// transport layer to derive an HTTP status, decide retryability, and render a
// client-facing message without inspecting the failure site.
package errs

import (
	"errors"
	"fmt"
)

// Kind classifies a failure by who can act on it and whether retrying helps.
type Kind int

const (
	KindInternal Kind = iota
	KindInvalidArgument
	KindUnauthenticated
	KindPermissionDenied
	KindNotFound
	KindConflict
	KindRateLimited
	KindUnavailable
	KindTimeout
	KindUpstream
)

func (k Kind) String() string {
	if int(k) >= 0 && int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return kindNames[KindInternal]
}

// Retryable reports whether replaying the identical request could succeed.
// Handlers use this to decide between a 503 and a 4xx without a policy table.
func (k Kind) Retryable() bool {
	switch k {
	case KindUnavailable, KindTimeout, KindUpstream, KindInternal:
		return true
	default:
		return false
	}
}

var kindNames = [...]string{
	"internal",
	"invalid_argument",
	"unauthenticated",
	"permission_denied",
	"not_found",
	"conflict",
	"rate_limited",
	"unavailable",
	"timeout",
	"upstream",
}

// Error is the only error type that crosses package boundaries in this repo.
// A bare error from any dependency must be wrapped before it reaches a handler.
type Error struct {
	Kind Kind
	// Code is a stable, machine-readable identifier that clients switch on.
	// Msg is for humans and may change without notice.
	Code string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Msg + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an error with a formatted message.
func New(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap annotates err without losing it, so errors.Is and errors.As keep
// working through the chain.
func Wrap(err error, kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: code, Msg: msg, Err: err}
}

// Of extracts the *Error from an error chain. ok is false when err did not
// originate here, in which case callers should treat it as KindInternal and
// hide the message from clients.
func Of(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// KindOf reports the classification of err, defaulting to KindInternal.
func KindOf(err error) Kind {
	if e, ok := Of(err); ok {
		return e.Kind
	}
	return KindInternal
}

// CodeOf reports the machine-readable code of err, or "" if unknown.
func CodeOf(err error) string {
	if e, ok := Of(err); ok {
		return e.Code
	}
	return ""
}

// Shorthands for the errors middleware returns on nearly every request path.
func Unauthenticated(format string, args ...any) *Error {
	return New(KindUnauthenticated, "invalid_api_key", format, args...)
}

func PermissionDenied(format string, args ...any) *Error {
	return New(KindPermissionDenied, "permission_denied", format, args...)
}

func InvalidArgument(format string, args ...any) *Error {
	return New(KindInvalidArgument, "invalid_request_error", format, args...)
}

func RateLimited(format string, args ...any) *Error {
	return New(KindRateLimited, "rate_limit_error", format, args...)
}

func NotFound(format string, args ...any) *Error {
	return New(KindNotFound, "not_found", format, args...)
}

func Unavailable(format string, args ...any) *Error {
	return New(KindUnavailable, "service_unavailable", format, args...)
}

// Upstream marks a failure that originated in an inference engine rather than
// in us. Callers need this to distinguish "our bug" from "the model is down".
func Upstream(err error, format string, args ...any) *Error {
	return Wrap(err, KindUpstream, "upstream_error", fmt.Sprintf(format, args...))
}

// Internal hides the wrapped error from clients but keeps it in the log chain.
func Internal(err error) *Error {
	return Wrap(err, KindInternal, "internal_error", "internal error")
}
