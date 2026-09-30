// Every call the page makes to the gateway. Kept apart from main.js so that
// the transport details live in one place and the UI code reads like UI code.

/** Settings the user can change at runtime; persisted in localStorage. */
export const settings = {
  baseUrl: localStorage.getItem('fleet.baseUrl') || location.origin,
  apiKey:  localStorage.getItem('fleet.apiKey')  || '',
};

export function saveSettings(patch) {
  Object.assign(settings, patch);
  for (const [k, v] of Object.entries(patch)) {
    localStorage.setItem('fleet.' + k, v);
  }
}

function headers(extra = {}) {
  const h = { 'Content-Type': 'application/json', ...extra };
  if (settings.apiKey) h.Authorization = 'Bearer ' + settings.apiKey;
  return h;
}

/** An error carrying the gateway's OpenAI error envelope where there is one. */
export class GatewayError extends Error {
  constructor(message, status, code) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function toError(res) {
  let message = res.statusText;
  let code = '';
  try {
    const body = await res.json();
    if (body?.error?.message) {
      message = body.error.message;
      code = body.error.code || '';
    }
  } catch {
    // Not an OpenAI error envelope; the status text is all we have.
  }
  return new GatewayError(message, res.status, code);
}

/** List the models the gateway is willing to serve. */
export async function listModels(signal) {
  const res = await fetch(new URL('/v1/models', settings.baseUrl), {
    headers: headers(), signal,
  });
  if (!res.ok) throw await toError(res);
  return (await res.json()).data ?? [];
}

/** Fleet-specific status: edition, entitlements, endpoint health. */
export async function fleetStatus(signal) {
  const res = await fetch(new URL('/fleet/status', settings.baseUrl), {
    headers: headers(), signal,
  });
  if (!res.ok) throw await toError(res);
  return res.json();
}

/**
 * Open a streaming chat completion. Returns the response so the caller can
 * read its body; the AbortSignal is what stops generation.
 */
export async function streamChat(body, signal) {
  const res = await fetch(new URL('/v1/chat/completions', settings.baseUrl), {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw await toError(res);
  if (!res.body) throw new GatewayError('response had no body', res.status);
  return res.body;
}

/**
 * The request body Fleet expects. include_usage is what makes a stream
 * billable: without it the engine never sends token counts and the gateway
 * has to fall back to charging max_tokens.
 */
export function chatBody({ model, messages, maxTokens, temperature, stream }) {
  return {
    model,
    messages,
    max_tokens: maxTokens,
    temperature,
    stream,
    stream_options: { include_usage: true },
  };
}
