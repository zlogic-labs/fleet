import { Tag, Typography } from 'antd';
import { Bubble, Prompts, Welcome } from '@ant-design/x';
import type { BubbleItemType } from '@ant-design/x';
import { RobotOutlined } from '@ant-design/icons';

import type { Turn } from './useChat';

const { Text } = Typography;

const STARTERS = [
  'Summarise what an inference endpoint is, in two sentences.',
  'Write a Python function that reads a JSONL file and groups rows by a key.',
  'Name three tradeoffs between horizontal replica scaling and adding GPUs to one model.',
];

// The typing effect animates text that arrives all at once. A streamed reply
// already arrives token by token, so replaying it would only add lag; the
// cursor is ours and lives in contentRender instead.
const ROLE = {
  user: { placement: 'end', variant: 'filled', shape: 'corner', avatar: null },
  ai: {
    placement: 'start',
    variant: 'outlined',
    shape: 'corner',
    avatar: null,
    contentRender: (content: string, info: { extraInfo?: unknown }) => {
      const turn = (info.extraInfo as { turn?: Turn } | undefined)?.turn;
      return (
        <>
          {content}
          {turn?.streaming && <span className="fleet-cursor" />}
        </>
      );
    },
  },
} as const;

function items(turns: Turn[]): BubbleItemType[] {
  return turns.map((turn) => ({
    key: turn.key,
    role: turn.role === 'user' ? 'user' : 'ai',
    content: turn.content,
    streaming: Boolean(turn.streaming),
    extraInfo: { turn },
    footer: turn.role === 'assistant' ? () => <TurnFooter turn={turn} /> : undefined,
  }));
}

function TurnFooter({ turn }: { turn: Turn }) {
  return (
    <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center', marginTop: 4 }}>
      {turn.error && <Text type="danger">{turn.error}</Text>}
      {turn.aborted && <Text type="warning">stopped by you</Text>}
      {turn.meta && (
        <>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {turn.meta.totalMs} ms · ttft {turn.meta.ttftMs} ms
          </Text>
          {turn.meta.finishReason && <Tag style={{ fontSize: 11 }}>{turn.meta.finishReason}</Tag>}
          {turn.meta.usage ? (
            <Tag color="green" style={{ fontSize: 11 }}>
              {turn.meta.usage.promptTokens}→{turn.meta.usage.completionTokens} tok
              {turn.meta.usage.cached ? ` · ${turn.meta.usage.cached} cached` : ''}
            </Tag>
          ) : (
            <Tag color="warning" style={{ fontSize: 11 }}>
              no usage reported · charged max_tokens
            </Tag>
          )}
        </>
      )}
    </div>
  );
}

export function Transcript({
  turns,
  onPick,
}: {
  turns: Turn[];
  onPick: (text: string) => void;
}) {
  if (turns.length === 0) {
    return (
      <div
        style={{
          height: '100%',
          display: 'grid',
          placeContent: 'center',
          justifyItems: 'center',
          gap: 24,
          padding: 24,
        }}
      >
        <Welcome
          icon={<RobotOutlined style={{ fontSize: 48 }} />}
          title="Nothing sent yet"
          description="Pick a starter or write your own. The reply streams from whatever replica the gateway picks."
        />
        <Prompts
          items={STARTERS.map((label) => ({ key: label, label }))}
          wrap
          onItemClick={({ data }) => onPick(String(data.label))}
        />
      </div>
    );
  }

  return (
    <Bubble.List
      autoScroll
      items={items(turns)}
      role={ROLE}
      style={{ height: '100%', padding: 20 }}
    />
  );
}