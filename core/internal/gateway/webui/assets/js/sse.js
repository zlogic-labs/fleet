// Server-sent event framing, reading from a fetch body stream.
//
// EventSource cannot be used: it is GET-only and the gateway requires the API
// key in an Authorization header, so a streaming chat completion has to go
// through fetch and be decoded by hand.

// A frame ends at a blank line. Both LF and CRLF are accepted because a proxy
// sitting between the browser and the gateway may rewrite line endings.
const FRAME_BOUNDARY = /\r?\n\r?\n/;

/**
 * Decode one SSE frame into its data payload, or null if it carries none.
 * Per the SSE spec several data: lines concatenate with newlines; OpenAI
 * streams send one per frame, but joining is free and avoids a subtle bug if
 * that ever changes.
 */
function frameData(frame) {
  let out = null;
  for (const raw of frame.split(/\r?\n/)) {
    if (!raw.startsWith('data:')) continue;
    const value = raw.slice(5).replace(/^ /, '');
    out = out === null ? value : out + '\n' + value;
  }
  return out;
}

/**
 * Yield the data payload of every complete frame in a response body.
 *
 * Frames split across TCP segments are handled by keeping the partial tail in
 * buf, which is the same problem the Go tap has to solve on the other side.
 */
export async function* sseFrames(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      // The stream flag keeps multi-byte characters intact across chunks.
      buf += decoder.decode(value, { stream: true });

      for (;;) {
        const m = FRAME_BOUNDARY.exec(buf);
        if (!m) break;
        const data = frameData(buf.slice(0, m.index));
        buf = buf.slice(m.index + m[0].length);
        if (data !== null) yield data;
      }
    }
  } finally {
    // Releasing the lock lets an abort actually cancel the request rather
    // than leaving it hanging until the engine finishes generating.
    reader.releaseLock();
  }
}

/**
 * Consume a text/event-stream body, calling onDelta for each content delta
 * and returning the final usage object if the engine sent one.
 *
 * A stream that ends without usage is not an error: the gateway caps such a
 * request at max_tokens and marks it estimated, which the caller surfaces.
 */
export async function consumeStream(body, onDelta) {
  let text = '';
  let usage = null;
  let finishReason = null;

  for await (const payload of sseFrames(body)) {
    if (payload === '[DONE]') break;
    if (!payload.startsWith('{')) continue;

    let chunk;
    try {
      chunk = JSON.parse(payload);
    } catch {
      continue; // A malformed frame is not worth failing the whole response.
    }

    if (chunk.usage) usage = chunk.usage;

    const delta = chunk.choices?.[0]?.delta?.content;
    if (delta) {
      text += delta;
      onDelta(delta);
    }
    const reason = chunk.choices?.[0]?.finish_reason;
    if (reason) finishReason = reason;
  }

  return { text, usage, finishReason };
}
