package openai

import (
	"encoding/json"
	"net/http"

	"github.com/zlogic/fleet/core/pkg/errs"
)

// ErrorEnvelope is the error shape every OpenAI-compatible client expects.
// The official SDKs read .error.type and .error.code, so both must be
// populated or they fall back to a generic exception.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// errorTypes maps a failure kind onto the coarse type the OpenAI schema defines.
// Matching the SDK's vocabulary matters more than matching our own vocabulary:
// a client that cannot classify the error will retry a request it should not.
var errorTypes = map[errs.Kind]string{
	errs.KindInvalidArgument:  "invalid_request_error",
	errs.KindUnauthenticated:  "invalid_request_error",
	errs.KindPermissionDenied: "invalid_request_error",
	errs.KindNotFound:         "invalid_request_error",
	errs.KindConflict:         "invalid_request_error",
	errs.KindRateLimited:      "rate_limit_error",
	errs.KindUnavailable:      "server_error",
	errs.KindTimeout:          "server_error",
	errs.KindUpstream:         "server_error",
	errs.KindInternal:         "server_error",
}

// ErrorBodyOf renders err into the wire shape. The message is filtered through
// errs so that an unwrapped error from a dependency never reaches a client.
func ErrorBodyOf(err error) ErrorBody {
	kind := errs.KindOf(err)
	body := ErrorBody{
		Message: errs.PublicMessage(err),
		Type:    errorTypes[kind],
	}
	if code := errs.CodeOf(err); code != "" {
		body.Code = &code
	}
	return body
}

// WriteError emits err using the OpenAI error envelope and the status code
// implied by its kind. It never writes a body twice; handlers that have
// already streamed must not call it at all.
func WriteError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errs.HTTPStatus(err))
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{Error: ErrorBodyOf(err)})
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
