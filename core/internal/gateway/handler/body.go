package handler

import "bytes"

// stream_options injection.
//
// Separate from the handler because this is the one piece of protocol surgery
// in the request path, and it is the piece with a correctness argument attached
// (see ensureIncludeUsage). Keeping it alone makes that argument reviewable
// without reading the reservation logic around it.

// includeUsageField is what Fleet inserts so that a streamed completion
// carries token counts at all. Without it the engine sends deltas only, the
// response is unbillable, and P6 forces the gateway to charge max_tokens —
// which is worse for the customer than Fleet setting the flag itself.
const includeUsageField = `"stream_options":{"include_usage":true}`

// ensureIncludeUsage returns body with stream_options.include_usage set.
//
// The common case is a surgical byte edit: a well-formed JSON object's last
// byte is its closing brace, so inserting before it costs nothing and keeps
// every other byte — including fields this struct does not model — intact.
// Re-marshalling instead would reorder keys and can render numbers
// differently, which is a poor trade for a gateway.
//
// Left alone, deliberately, when the body does not look like a JSON object. A
// client that sent something this cannot parse will get a clearer error from
// the engine than from a rewrite that guessed at its shape.
func ensureIncludeUsage(body []byte) []byte {
	if bytes.Contains(body, []byte(`"include_usage":true`)) {
		return body
	}
	trimmed := bytes.TrimRight(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return body
	}
	inner := bytes.TrimRight(trimmed[:len(trimmed)-1], " \t\r\n")

	out := make([]byte, 0, len(trimmed)+len(includeUsageField)+1)
	if len(inner) == 0 {
		out = append(out, '{')
	} else {
		out = append(out, inner...)
		out = append(out, ',')
	}
	out = append(out, includeUsageField...)
	out = append(out, '}')
	return out
}
