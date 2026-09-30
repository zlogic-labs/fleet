/** Calls against the gateway: the OpenAI surface plus /fleet/status. */

import { request, requestOrNull, origins, apiKey } from './client';
import type { FleetStatus, Model } from '../types';

export function listModels(signal?: AbortSignal) {
  return request<{ data: Model[] }>('/v1/models', { group: 'gateway', signal }).then((r) => r.data);
}

export function fleetStatus(signal?: AbortSignal) {
  return requestOrNull<FleetStatus>('/fleet/status', { group: 'gateway', signal });
}

export interface ChatMessage {
  role: 'user' | 'assistant' | 'system';
  content: string;
}

export interface ChatRequest {
  model: string;
  messages: ChatMessage[];
  max_tokens: number;
  temperature: number;
  stream: boolean;
  stream_options?: { include_usage: boolean };
}

/**
 * Open a streaming completion.
 *
 * The body is built here rather than by the caller so that the console never
 * sends a request Fleet would have to repair: include_usage is Fleet's to set,
 * and a client that omits it gets a stream with no billable token counts.
 */
export function streamChat(
  body: Omit<ChatRequest, 'stream' | 'stream_options'>,
  signal: AbortSignal,
): Promise<Response> {
  return fetch(`${origins.gateway}/v1/chat/completions`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(apiKey() ? { Authorization: `Bearer ${apiKey()}` } : {}),
    },
    body: JSON.stringify({
      ...body,
      stream: true,
      stream_options: { include_usage: true },
    }),
    signal,
  });
}

export async function checkGateway(): Promise<{ ok: boolean; detail: string }> {
  try {
    const models = await listModels();
    return { ok: true, detail: `${models.length} model${models.length === 1 ? '' : 's'}` };
  } catch {
    return { ok: false, detail: 'unreachable' };
  }
}
