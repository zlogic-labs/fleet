/** Server-sent event framing over a fetch body stream.
 *
 * EventSource is unusable here: it is GET-only, and the gateway needs the API
 * key in an Authorization header. The browser-side twin of the Go tap, with
 * the same obligation to buffer a frame that straddles two TCP segments.
 */

const FRAME_BOUNDARY = /\r?\n\r?\n/;

function frameData(frame: string): string | null {
  let out: string | null = null;
  for (const raw of frame.split(/\r?\n/)) {
    if (!raw.startsWith('data:')) continue;
    const value = raw.slice(5).replace(/^ /, '');
    out = out === null ? value : `${out}\n${value}`;
  }
  return out;
}

export interface StreamChunk {
  content?: string;
  finishReason?: string | null;
  usage?: {
    prompt_tokens: number;
    completion_tokens: number;
    total_tokens: number;
    prompt_tokens_details?: { cached_tokens?: number };
  };
}

async function* sseFrames(body: ReadableStream<Uint8Array>): AsyncGenerator<string> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      // The stream flag keeps a multi-byte character intact across chunks.
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
    // Releasing the lock lets an abort actually cancel the request instead of
    // leaving it hanging until the engine finishes generating.
    reader.releaseLock();
  }
}

export interface StreamResult {
  text: string;
  usage: StreamChunk['usage'];
  finishReason: string | null;
}

/**
 * Consume a text/event-stream body, calling onDelta for each content delta.
 *
 * A stream that ends without usage is not an error: the gateway caps such a
 * request at max_tokens and marks the settlement as estimated, which the
 * caller is expected to surface rather than hide.
 */
export async function consumeStream(
  body: ReadableStream<Uint8Array>,
  onDelta: (delta: string) => void,
): Promise<StreamResult> {
  let text = '';
  let usage: StreamChunk['usage'];
  let finishReason: string | null = null;

  for await (const payload of sseFrames(body)) {
    if (payload === '[DONE]') break;
    if (!payload.startsWith('{')) continue;

    let chunk: StreamChunk;
    try {
      chunk = JSON.parse(payload) as StreamChunk;
    } catch {
      continue; // One malformed frame is not worth failing the whole answer.
    }

    if (chunk.usage) usage = chunk.usage;

    const choices = (chunk as { choices?: Array<{ delta?: { content?: string }; finish_reason?: string }> })
      .choices;
    const choice = choices?.[0];
    if (choice?.delta?.content) {
      text += choice.delta.content;
      onDelta(choice.delta.content);
    }
    if (choice?.finish_reason) finishReason = choice.finish_reason;
  }

  return { text, usage, finishReason };
}
