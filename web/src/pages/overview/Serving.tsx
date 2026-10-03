import { Typography } from 'antd';

import { Group, Measures } from './parts';
import type { FleetStatus } from '../../types';

const { Text } = Typography;

// What is actually serving, and how hard it is working.
//
// These four came off the old Fleet page, where they sat in four identical
// boxes that made a healthy idle system look identical to a busy one. The
// numbers that matter are the ones under pressure: a queue and a running count
// say what is happening now, while a latency average across recent requests is
// context for it.

export function Serving({ status }: { status: FleetStatus | null | undefined }) {
  const eps = status?.endpoints ?? [];
  const healthy = eps.filter((e) => e.healthy).length;
  const queue = eps.reduce((a, e) => a + (e.queueDepth ?? 0), 0);
  const running = eps.reduce((a, e) => a + (e.runningRequests ?? 0), 0);

  const recent = status?.recent ?? [];
  const ttft = recent.length
    ? Math.round(recent.reduce((a, s) => a + s.ttftMs, 0) / recent.length)
    : 0;
  const completion = recent.reduce((a, s) => a + s.completionTokens, 0);
  const prompt = recent.reduce((a, s) => a + s.promptTokens, 0);
  const cached = recent.reduce((a, s) => a + s.cachedTokens, 0);

  if (!status) {
    return (
      <Group title="Serving">
        <Text type="secondary">The gateway has not reported its endpoints.</Text>
      </Group>
    );
  }

  return (
    <Group title="Serving" to={{ path: '/fleet', label: 'endpoints' }}>
      <Measures
        items={[
          {
            label: 'Endpoints',
            value: `${healthy} of ${eps.length} healthy`,
            hint: eps.length ? eps.map((e) => e.model).slice(0, 2).join(', ') : undefined,
          },
          { label: 'Waiting', value: String(queue), hint: 'queued at an engine' },
          { label: 'In flight', value: String(running), hint: 'running at an engine' },
          {
            label: 'Avg time to first token',
            value: recent.length ? `${ttft} ms` : '—',
            hint: `${recent.length} recent request${recent.length === 1 ? '' : 's'}`,
          },
          {
            label: 'Prompt tokens',
            value: recent.length ? prompt.toLocaleString() : '—',
            hint: cached > 0 ? `${cached.toLocaleString()} from cache` : undefined,
          },
          {
            label: 'Completion tokens',
            value: recent.length ? completion.toLocaleString() : '—',
          },
        ]}
      />
    </Group>
  );
}