import { useCallback, useEffect, useRef, useState } from 'react';
import {
  App as AntApp,
  Button,
  Card,
  Empty,
  Input,
  Select,
  Slider,
  Space,
  Tag,
  Typography,
} from 'antd';
import { ClearOutlined, SendOutlined, StopOutlined } from '@ant-design/icons';

import { streamChat, type ChatMessage } from '../api/gateway';
import { consumeStream } from '../api/sse';
import { ApiError, errorText } from '../api/client';
import type { Model } from '../types';

const { Text, Paragraph } = Typography;
const { TextArea } = Input;

interface Turn {
  key: string;
  role: 'user' | 'assistant';
  content: string;
  streaming?: boolean;
  aborted?: boolean;
  error?: string;
  meta?: {
    totalMs: number;
    ttftMs: number;
    finishReason: string | null;
    usage?: { prompt_tokens: number; completion_tokens: number; cached?: number };
  };
}

let turnSeq = 0;
const nextKey = () => `t${++turnSeq}`;

export function Playground({ models }: { models: Model[] }) {
  const { message } = AntApp.useApp();
  const [model, setModel] = useState<string>('');
  const [maxTokens, setMaxTokens] = useState(1024);
  const [temperature, setTemperature] = useState(0.7);
  const [turns, setTurns] = useState<Turn[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);

  const abortRef = useRef<AbortController | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);

  const history = useRef<ChatMessage[]>([]);

  useEffect(() => {
    if (!model && models[0]?.id) setModel(models[0].id);
  }, [models, model]);

  useEffect(() => {
    const el = transcriptRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [turns]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const patch = useCallback((key: string, next: Partial<Turn>) => {
    setTurns((prev) => prev.map((t) => (t.key === key ? { ...t, ...next } : t)));
  }, []);

  const send = useCallback(async () => {
    const text = input.trim();
    if (!text || busy) return;
    if (!model) {
      message.warning('no model selected');
      return;
    }

    const userTurn: Turn = { key: nextKey(), role: 'user', content: text };
    const replyKey = nextKey();
    const replyTurn: Turn = { key: replyKey, role: 'assistant', content: '', streaming: true };
    setTurns((prev) => [...prev, userTurn, replyTurn]);
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
      // the `turns` of the render that started the request, so reading it back
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
                prompt_tokens: result.usage.prompt_tokens,
                completion_tokens: result.usage.completion_tokens,
                cached: result.usage.prompt_tokens_details?.cached_tokens,
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
  }, [busy, input, maxTokens, message, model, patch, temperature]);

  const clear = () => {
    abortRef.current?.abort();
    history.current = [];
    setTurns([]);
  };

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '280px 1fr', height: '100%', minHeight: 0 }}>
      <Card size="small" title="Request" style={{ borderRadius: 0, borderInlineEnd: 0 }}>
        <Space direction="vertical" size="large" style={{ width: '100%' }}>
          <label style={{ display: 'block' }}>
            <Text type="secondary" style={{ fontSize: 12 }}>
              Model
            </Text>
            <Select
              value={model || undefined}
              placeholder="no models served"
              onChange={setModel}
              style={{ width: '100%', marginTop: 4 }}
              options={models.map((m) => ({
                value: m.id,
                label: m.id,
              }))}
              notFoundContent="the gateway advertises no models"
            />
          </label>

          <div>
            <Text type="secondary" style={{ fontSize: 12 }}>
              Max tokens: <Text strong>{maxTokens}</Text>
            </Text>
            <Slider
              min={64}
              max={8192}
              step={64}
              value={maxTokens}
              onChange={setMaxTokens}
              tooltip={{ open: false }}
            />
          </div>

          <div>
            <Text type="secondary" style={{ fontSize: 12 }}>
              Temperature: <Text strong>{temperature.toFixed(1)}</Text>
            </Text>
            <Slider
              min={0}
              max={2}
              step={0.1}
              value={temperature}
              onChange={setTemperature}
              tooltip={{ open: false }}
            />
          </div>

          <Button icon={<ClearOutlined />} onClick={clear} disabled={turns.length === 0} block>
            Clear
          </Button>

          <Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 0 }}>
            Usage is metered from the engine's own <code>usage</code> field. A streamed response is
            only billable if the engine reports usage, which is why Fleet sets{' '}
            <code>stream_options.include_usage</code> itself.
          </Paragraph>
        </Space>
      </Card>

      <div style={{ display: 'grid', gridTemplateRows: '1fr auto', minHeight: 0 }}>
        <div ref={transcriptRef} style={{ overflowY: 'auto', padding: 20 }}>
          {turns.length === 0 ? (
            <div style={{ display: 'grid', placeItems: 'center', height: '100%' }}>
              <Empty description="Send a message to see a completion" />
            </div>
          ) : (
            <Space direction="vertical" size="middle" style={{ width: '100%' }}>
              {turns.map((turn) => (
                <TurnView key={turn.key} turn={turn} />
              ))}
            </Space>
          )}
        </div>

        <div
          style={{
            borderTop: '1px solid rgba(5,5,5,0.06)',
            padding: 12,
            display: 'flex',
            gap: 8,
            alignItems: 'flex-end',
          }}
        >
          <TextArea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Send a message…  (Enter to send, Shift+Enter for a newline)"
            autoSize={{ minRows: 1, maxRows: 8 }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                void send();
              }
            }}
            disabled={busy}
          />
          {busy ? (
            <Button danger icon={<StopOutlined />} onClick={() => abortRef.current?.abort()}>
              Stop
            </Button>
          ) : (
            <Button type="primary" icon={<SendOutlined />} onClick={() => void send()} disabled={!input.trim()}>
              Send
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}

function TurnView({ turn }: { turn: Turn }) {
  if (turn.role === 'user') {
    return (
      <div style={{ maxWidth: 720, marginLeft: 'auto' }}>
        <div
          style={{
            background: '#1677ff',
            color: '#fff',
            padding: '10px 14px',
            borderRadius: 10,
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >
          {turn.content}
        </div>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: 720 }}>
      <Space direction="vertical" size={4} style={{ width: '100%' }}>
        <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', minHeight: 24 }}>
          {turn.content}
          {turn.streaming && <span className="fleet-cursor" />}
        </div>

        {turn.error && <Text type="danger">{turn.error}</Text>}
        {turn.aborted && <Text type="warning">stopped by you</Text>}

        {turn.meta && (
          <Space size={8} wrap>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {turn.meta.totalMs} ms total · {turn.meta.ttftMs} ms ttft
            </Text>
            {turn.meta.finishReason && (
              <Tag style={{ fontSize: 11 }}>{turn.meta.finishReason}</Tag>
            )}
            {turn.meta.usage ? (
              <Tag color="green" style={{ fontSize: 11 }}>
                {turn.meta.usage.prompt_tokens}→{turn.meta.usage.completion_tokens} tok
                {turn.meta.usage.cached ? ` · ${turn.meta.usage.cached} cached` : ''}
              </Tag>
            ) : (
              <Tag color="warning" style={{ fontSize: 11 }}>
                no usage reported · charged max_tokens
              </Tag>
            )}
          </Space>
        )}
      </Space>
    </div>
  );
}
