package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/zlogic/fleet/core/pkg/errs"
	"github.com/zlogic/fleet/core/pkg/openai"
)

// maxErrorBody bounds how much of a failing response we read before deciding
// what to do with it. Engine errors are small; an HTML page from an
// intermediary is not, and buffering one to reach a conclusion would trade a
// clear error for an out-of-memory.
const maxErrorBody = 8 << 10

// rewriteServerError decides what the client sees when an engine returns a
// failing status.
//
// Only 5xx reaches here; a 4xx is forwarded untouched. The rule for a 5xx is
// that the body is often an HTML error page or an empty response from
// something in between, and forwarding that would leave the SDK unable to
// parse anything. An engine's own valid OpenAI error envelope is preserved
// as-is, because in that case the engine did explain itself.
func rewriteServerError(resp *http.Response) error {
	body, err := readBody(resp)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		return nil
	}

	var envelope openai.ErrorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}

	// Every 5xx from an engine means the same thing to a caller: this model
	// cannot serve right now. Reporting it as 503 rather than passing the
	// status through is what lets a client — or our own picker — retry
	// against a different replica instead of failing the request.
	kind := errs.KindUpstream
	if resp.StatusCode == http.StatusServiceUnavailable {
		// The model is loading or the pod is restarting, which is a
		// distinct and more actionable condition than a hard failure.
		kind = errs.KindUnavailable
	}

	detail := truncate(bytes.TrimSpace(body), 512)
	msg := "the inference engine reported an internal error"
	if len(detail) > 0 {
		msg += ": " + string(detail)
	}
	envelope.Error = openai.ErrorBodyOf(errs.New(kind, "upstream_error", "%s", msg))

	encoded, _ := json.Marshal(envelope)
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.StatusCode = http.StatusServiceUnavailable
	resp.Header.Set("Content-Type", "application/json")
	resp.ContentLength = int64(len(encoded))
	resp.Header.Del("Content-Length")
	return nil
}

func readBody(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
