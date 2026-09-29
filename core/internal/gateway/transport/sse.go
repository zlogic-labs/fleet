package transport

import "bytes"

// Server-sent event framing. A frame ends at a blank line, and a frame's
// payload is the concatenation of its "data:" lines. OpenAI-style streams
// emit exactly one data line per frame, but handling the general form costs
// almost nothing and avoids a subtle bug if a client ever adds a comment line.

// sseTerminators are checked in order. "\n\n" is what every engine Fleet
// supports actually emits; the CRLF form is here so that a proxy inserting
// itself upstream of us cannot desynchronise the scanner.
var sseTerminators = [][]byte{[]byte("\n\n"), []byte("\r\n\r\n")}

// sseBoundary returns the index just past the first complete frame in b, or -1
// if b does not yet contain one.
func sseBoundary(b []byte) int {
	best := -1
	for _, term := range sseTerminators {
		if i := bytes.Index(b, term); i >= 0 && (best < 0 || i+len(term) < best) {
			best = i + len(term)
		}
	}
	return best
}

// sseData returns the data payload of a single frame, or nil if the frame
// carries no data line (which is what a comment or retry directive looks
// like). The returned slice aliases frame; it is not copied.
func sseData(frame []byte) []byte {
	var out []byte
	for len(frame) > 0 {
		var line []byte
		if i := bytes.IndexByte(frame, '\n'); i >= 0 {
			line, frame = frame[:i], frame[i+1:]
		} else {
			line, frame = frame, nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))

		field, value, found := bytes.Cut(line, []byte(":"))
		if !found || !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if out != nil {
			out = append(out, '\n')
		}
		out = append(out, value...)
	}
	return out
}
