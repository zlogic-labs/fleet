import { Typography } from 'antd';

import { Pool } from './overview/Pool';
import { Serving } from './overview/Serving';
import { Spend } from './overview/Spend';
import { usePoll } from '../hooks';
import { cluster as clusterApi, deployments as deploymentsApi, costPeriods, spend } from '../api/control';
import { fleetStatus } from '../api/gateway';

const { Text } = Typography;

// The landing page.
//
// Six pages each answered one question and none of them answered the one an
// operator opens the console with: is the fleet busy, and what does it cost.
// Capacity lived under Infrastructure, load under Serving, money under Cost, and
// answering it meant holding three pages in your head at once. Idle GPUs are
// the thing this platform exists to make visible, and they were the one number
// no single screen showed.

export function Overview() {
  const now = Date.now();
  const status = usePoll((signal) => fleetStatus(signal), 5000);
  const clusters = usePoll((signal) => clusterApi.get(signal), 5000);
  const deps = usePoll((signal) => deploymentsApi.list(signal), 10000);
  const periods = usePoll((signal) => costPeriods.list(signal), 30000);
  // Faster than the closed periods: this figure moves with traffic, and an
  // operator watching spend would rather see it stale by seconds than by a
  // minute.
  const running = usePoll((signal) => spend.open(signal), 10000);

  const report = clusters.data?.clusters?.[0];
  const list = periods.data ?? [];

  return (
    <div>
      <Pool report={report} deployments={deps.data ?? []} now={now} />
      <Serving status={status.data} />
      <Spend
        periods={list}
        open={running.data}
        currency={
          list.find((p) => p.currency)?.currency ??
          // Nothing closed yet, so the open figure is the only thing that can
          // name a currency. A rate declared but not yet applied still counts.
          'USD'
        }
      />

      {(clusters.error || periods.error) && (
        <Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 18 }}>
          Some of this needs the control plane. Start fleet-apiserver, or set its address under
          Settings.
        </Text>
      )}
    </div>
  );
}