package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// usageMarker is the cheap gate that keeps the tap off the JSON parser for
// the overwhelming majority of frames. A streaming completion emits hundreds
// of deltas and at most one usage object, so scanning for this substring first
// removes effectively all decode cost from the hot path.
var usageMarker = []byte(`"usage"`)

// There is one decode path for every response shape, not one per endpoint.
//
// An embeddings reply is decoded as a chat completion and reads correctly:
// `usage` sits at the top level of both with the same shape, and encoding/json
// ignores the fields that differ (`data` and `object`). A second shape-specific
// path was written here first, on the belief that a chat decode would find no
// usage and silently fall back to billing the reservation. The test that
// assumed the opposite failed, which is how it was found.

// DefaultRetainCap bounds how many response bytes the tap keeps for a
// non-streaming parse. Beyond it the tap stops accumulating and the billing
// layer falls back to the P6 policy, which caps the request at max_tokens.
const DefaultRetainCap = 1 << 20

// Tap observes a proxied response without altering a single byte the client
// receives. It exists because billing needs facts the wire does not carry as
// structured data: when the first token arrived, whether the engine actually
// reported usage, and how many bytes crossed the proxy.
type Tap struct {
	cap   int
	clock func() time.Time

	sse       bool
	buf       []byte
	truncated bool
	bytes     int64

	usage    *openai.Usage
	sawUsage bool

	startedAt time.Time
	firstByte time.Duration
	sealed    bool
}

// Result is what the billing layer reads once the response has been written.
type Result struct {
	// Usage is the engine's own accounting. It is nil when the engine
	// reported none, which is a normal outcome for a stream cut short.
	Usage *openai.Usage
	// UsageKnown separates "engine said zero tokens" from "engine said
	// nothing", which are different billing outcomes.
	UsageKnown bool
	TTFT       time.Duration
	Duration   time.Duration
	Bytes      int64
}

// NewTap starts an observation. clock is injectable so tests need not sleep.
func NewTap(retainCap int, clock func() time.Time) *Tap {
	if retainCap <= 0 {
		retainCap = DefaultRetainCap
	}
	if clock == nil {
		clock = time.Now
	}
	return &Tap{cap: retainCap, clock: clock, startedAt: clock()}
}

// UseSSE selects the frame scanner. The proxy calls this from the response
// Content-Type rather than trusting the client's stream flag, because a
// client can ask for a stream and still be served JSON.
func (t *Tap) UseSSE(on bool) { t.sse = on }

// Reader wraps the upstream body. Reads pass through untouched; the tap only
// watches. Close is forwarded so the pooled connection is released normally.
func (t *Tap) Reader(rc io.ReadCloser) io.ReadCloser {
	return &tapReader{tap: t, rc: rc}
}

type tapReader struct {
	tap *Tap
	rc  io.ReadCloser
}

func (r *tapReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.tap.observe(p[:n])
	}
	if err != nil {
		r.tap.seal()
	}
	return n, err
}

func (r *tapReader) Close() error { return r.rc.Close() }

func (t *Tap) observe(b []byte) {
	if t.bytes == 0 {
		t.firstByte = t.clock().Sub(t.startedAt)
	}
	t.bytes += int64(len(b))

	if t.sse {
		t.buf = append(t.buf, b...)
		t.drainFrames()
		return
	}
	if len(t.buf) >= t.cap {
		t.truncated = true
		return
	}
	t.buf = append(t.buf, b...)
}

// drainFrames consumes every complete SSE frame in the buffer, leaving any
// partial frame for the next read. A frame may be split across TCP segments,
// so the remainder has to survive between calls.
func (t *Tap) drainFrames() {
	for {
		end := sseBoundary(t.buf)
		if end < 0 {
			return
		}
		frame := t.buf[:end]
		t.buf = t.buf[end:]
		if data := sseData(frame); len(data) > 0 {
			t.considerUsage(data)
		}
	}
}

func (t *Tap) considerUsage(payload []byte) {
	if t.sawUsage || !bytes.Contains(payload, usageMarker) {
		return
	}
	var chunk openai.ChatChunk
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return
	}
	t.usage, t.sawUsage = chunk.Usage, true
}

// seal runs the end-of-response path. It is idempotent, because a body can
// report both a non-nil error and a subsequent Close.
func (t *Tap) seal() {
	if t.sealed {
		return
	}
	t.sealed = true
	if t.sse || t.truncated || len(t.buf) == 0 || t.sawUsage {
		return
	}
	var resp openai.ChatResponse
	if err := json.Unmarshal(t.buf, &resp); err != nil {
		return
	}
	t.usage, t.sawUsage = resp.Usage, resp.Usage != nil
}

// Result is safe to call at any point, including after a client disconnect
// that never produced EOF. A partially filled Result is the expected input to
// the P6 fallback, not an error case.
func (t *Tap) Result() Result {
	return Result{
		Usage:      t.usage,
		UsageKnown: t.sawUsage,
		TTFT:       t.firstByte,
		Duration:   t.clock().Sub(t.startedAt),
		Bytes:      t.bytes,
	}
}
