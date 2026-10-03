import { useEffect, useState } from 'react';
import { Button, Card, Select, Slider, Space, Typography } from 'antd';
import { Sender } from '@ant-design/x';
import { ClearOutlined } from '@ant-design/icons';

import { Transcript } from './playground/Transcript';
import { useChat } from './playground/useChat';
import type { Model } from '../types';

const { Text, Paragraph } = Typography;

export function Playground({ models }: { models: Model[] }) {
  const [model, setModel] = useState<string>('');
  const [maxTokens, setMaxTokens] = useState(1024);
  const [temperature, setTemperature] = useState(0.7);
  const chat = useChat(model, maxTokens, temperature);

  useEffect(() => {
    if (!model && models[0]?.id) setModel(models[0].id);
  }, [models, model]);

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '280px 1fr', height: '100%', minHeight: 0 }}>
      <Card size="small" title="Request" style={{ borderRadius: 0, borderInlineEnd: 0 }}>
        <Space direction="vertical" size="large" style={{ width: '100%' }}>
          <div>
            <Text type="secondary" style={{ fontSize: 12 }}>
              Model
            </Text>
            <Select
              value={model || undefined}
              placeholder="no models served"
              onChange={setModel}
              style={{ width: '100%', marginTop: 4 }}
              options={models.map((m) => ({ value: m.id, label: m.id }))}
              notFoundContent="the gateway advertises no models"
            />
          </div>

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

          <Button
            icon={<ClearOutlined />}
            onClick={chat.clear}
            disabled={chat.turns.length === 0}
            block
          >
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
        <div style={{ minHeight: 0, overflow: 'hidden' }}>
          <Transcript turns={chat.turns} onPick={(text) => void chat.send(text)} />
        </div>

        <div style={{ borderTop: '1px solid rgba(5,5,5,0.06)', padding: 12 }}>
          <Sender
            value={chat.input}
            onChange={chat.setInput}
            onSubmit={(text) => void chat.send(text)}
            onCancel={chat.stop}
            loading={chat.busy}
            submitType="enter"
            placeholder="Send a message…  (Enter to send, Shift+Enter for a newline)"
          />
        </div>
      </div>
    </div>
  );
}