import { useCallback, useEffect, useRef, useState } from 'react';
import { App as AntApp } from 'antd';

import { streamChat, type ChatMessage } from '../../api/gateway';
import { consumeStream } from '../../api/sse';
import { ApiError, errorText } from '../../api/client';

export interface TurnMeta {
  totalMs: number;
  ttftMs: number;
  finishReason: string | null;
  usage?: { promptTokens: number; completionTokens: number; cached: number };
}

export interface Turn {
  key: string;
  role: 'user' | 'assistant';
  content: string;
  streaming?: boolean;
  aborted?: boolean;
  error?: string;
  meta?: TurnMeta;
}

let seq = 0;
const nextKey = () => `t${++seq}`;

export function useChat(model: string, maxTokens: number, temperature: number) {
  const { message } = AntApp.useApp();
  const [turns, setTurns] = useState<Turn[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);

  const abortRef = useRef<AbortController | null>(null);
  const history = useRef<ChatMessage[]>([]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const patch = useCallback((key: string, next: Partial<Turn>) => {
    setTurns((prev) => prev.map((t) => (t.key === key ? { ...t, ...next } : t)));
  }, []);

  const send = useCallback(
    async (draft?: string) => {
      const text = (draft ?? input).trim();
      if (!text || busy) return;
      if (!model) {
        message.warning('no model selected');
        return;
      }

      const replyKey = nextKey();
      setTurns((prev) => [
        ...prev,
        { key: nextKey(), role: 'user', content: text },
        { key: replyKey, role: 'assistant', content: '', streaming: true },
      ]);
      setInput('');
      setBusy(true);

      history.current = [...history.current, { role: 'user', content: text }];

      const controller = new AbortController();
      abortRef.current = controller;
      const started = performance.now();
      let firstByteAt = 0;

      try {
        const res = await streamChat(
          { model, messages: history.current, max_tokens: maxTokens, temperature },
          controller.signal,
        );
        if (!res.ok) {
          let detail = res.statusText;
          try {
            const body = await res.json();
            if (body?.error?.message) detail = body.error.message;
          } catch {
            // Not an envelope; the status text stands.
          }
          throw new ApiError(detail, res.status, '', 'server_error');
        }
        if (!res.body) throw new ApiError('response had no body', res.status, '', 'server_error');

        // Accumulate in a local, not in state: the delta callback closes over
        // the state of the render that started the request, so reading it back
        // would drop every token but the first after that render.
        let acc = '';
        const result = await consumeStream(res.body, (delta) => {
          if (!firstByteAt) firstByteAt = performance.now();
          acc += delta;
          patch(replyKey, { content: acc });
        });

        history.current = [...history.current, { role: 'assistant', content: result.text }];
        patch(replyKey, {
          content: result.text,
          streaming: false,
          meta: {
            totalMs: Math.round(performance.now() - started),
            ttftMs: Math.round((firstByteAt || performance.now()) - started),
            finishReason: result.finishReason,
            usage: result.usage
              ? {
                  promptTokens: result.usage.prompt_tokens,
                  completionTokens: result.usage.completion_tokens,
                  cached: result.usage.prompt_tokens_details?.cached_tokens ?? 0,
                }
              : undefined,
          },
        });
      } catch (err) {
        const aborted = err instanceof DOMException && err.name === 'AbortError';
        patch(replyKey, {
          streaming: false,
          aborted,
          content: aborted ? '(stopped)' : '',
          error: aborted ? undefined : errorText(err),
        });
        if (!aborted) message.error(errorText(err));
      } finally {
        abortRef.current = null;
        setBusy(false);
      }
    },
    [busy, input, maxTokens, message, model, patch, temperature],
  );

  const stop = useCallback(() => abortRef.current?.abort(), []);

  const clear = useCallback(() => {
    abortRef.current?.abort();
    history.current = [];
    setTurns([]);
    setInput('');
  }, []);

  return { turns, input, setInput, busy, send, stop, clear };
}