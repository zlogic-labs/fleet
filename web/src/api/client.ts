/**
 * One fetch wrapper for both the gateway and the control plane.
 *
 * Every backend answers errors with an OpenAI error envelope, so parsing it
 * once here means a page can show the server's own message instead of
 * "something went wrong".
 */

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly kind: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }

  /** True when the failure is the control plane being absent, not an error. */
  get isUnreachable(): boolean {
    return this.status === 0;
  }
}

/**
 * baseFor picks the origin for a path group. The console can be served by the
 * gateway itself, by the control plane, or by a dev server on a third port, so
 * the origin is configurable per group rather than assumed to be one.
 */
export const origins = {
  gateway: localStorage.getItem('fleet.gatewayUrl') || '',
  control: localStorage.getItem('fleet.controlUrl') || 'http://127.0.0.1:8081',
};

export function setOrigin(group: 'gateway' | 'control', value: string) {
  origins[group] = value.replace(/\/+$/, '');
  localStorage.setItem(`fleet.${group}Url`, origins[group]);
}

export function apiKey(): string {
  return localStorage.getItem('fleet.apiKey') || '';
}

export function setApiKey(value: string) {
  localStorage.setItem('fleet.apiKey', value);
}

async function toError(res: Response): Promise<ApiError> {
  let message = res.statusText || `HTTP ${res.status}`;
  let code = '';
  let kind = 'server_error';
  try {
    const body = await res.json();
    if (body?.error?.message) {
      message = body.error.message;
      code = body.error.code || '';
      kind = body.error.type || kind;
    }
  } catch {
    // Not an envelope: the status line is all there is.
  }
  return new ApiError(message, res.status, code, kind);
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  group?: 'gateway' | 'control';
  signal?: AbortSignal;
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, group = 'control', signal } = opts;
  const base = group === 'gateway' ? origins.gateway : origins.control;
  const key = apiKey();

  const headers: Record<string, string> = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (key) headers.Authorization = `Bearer ${key}`;

  let res: Response;
  try {
    res = await fetch(base + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (err) {
    if (signal?.aborted) throw err;
    // Status 0 marks "never reached it", which the console renders as an
    // empty state rather than an error: a control plane that is not running
    // yet is a normal state for a fresh checkout.
    throw new ApiError(
      `cannot reach ${base || 'the gateway'}`,
      0,
      'unreachable',
      'network_error',
    );
  }

  if (!res.ok) throw await toError(res);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** GET that resolves to null instead of throwing when the service is absent. */
export async function requestOrNull<T>(path: string, opts: RequestOptions = {}): Promise<T | null> {
  try {
    return await request<T>(path, opts);
  } catch (err) {
    if (err instanceof ApiError && err.isUnreachable) return null;
    throw err;
  }
}

export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error) return err.message;
  return String(err);
}
