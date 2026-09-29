package errs

import "net/http"

// HTTPStatus maps an error to the status code an OpenAI-compatible client
// expects. Keeping the mapping here means no handler has to remember it.
func HTTPStatus(err error) int {
	switch KindOf(err) {
	case KindInvalidArgument:
		return http.StatusBadRequest
	case KindUnauthenticated:
		return http.StatusUnauthorized
	case KindPermissionDenied:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindUnavailable:
		return http.StatusServiceUnavailable
	case KindTimeout:
		return http.StatusGatewayTimeout
	case KindUpstream:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// PublicMessage decides what a client is allowed to learn. An error that did
// not originate in errs is treated as ours, so its message is replaced with a
// generic one; the real cause goes to the log, not to the caller.
func PublicMessage(err error) string {
	if e, ok := Of(err); ok {
		return e.Msg
	}
	return "internal error"
}
