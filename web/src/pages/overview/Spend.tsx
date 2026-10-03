import { Table, Typography } from 'antd';
import { Link } from 'react-router-dom';

import { humanMoney } from '../../hooks';
import type { CostPeriodSummary } from '../../types';

const { Text } = Typography;

// What the fleet costs.
//
// Only closed periods have a figure, and that is not a gap in the page: a
// period's price depends on the whole month's capacity samples and the rate
// standing at the close, so it cannot be stated before then. The open period is
// therefore shown as open rather than as zero — a zero would be read as "this
// month was free", which is the one thing it must never say.

export function Spend({ periods, currency }: { periods: CostPeriodSummary[]; currency: string }) {
  const closed = periods.slice().sort((a, b) => b.period.localeCompare(a.period));

  return (
    <section style={{ marginTop: 32 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12 }}>
        <Text strong style={{ fontSize: 12, letterSpacing: '.06em', textTransform: 'uppercase' }}>
          Spend
        </Text>
        <span style={{ flex: 1 }} />
        <Link to="/cost" style={{ fontSize: 12 }}>
          cost →
        </Link>
      </div>

      {closed.length === 0 ? (
        // No table, because a header row over zero rows is furniture for an
        // absence. Most deployments are here for their first month.
        <Text type="secondary" style={{ fontSize: 13 }}>
          {currentPeriod()} is open. Fleet prices a period when it closes, so there is no figure for
          it yet — the token ledger is already recording every request against it.
        </Text>
      ) : (
        <>
          <Table
            style={{ marginTop: 10 }}
            size="small"
            pagination={false}
            rowKey="period"
            dataSource={closed}
            columns={[
              { title: 'Period', dataIndex: 'period', width: 120 },
              {
                title: 'Pool',
                width: 140,
                render: (_, p) =>
                  p.priced ? (
                    humanMoney(p.pool, currency)
                  ) : (
                    <Text type="secondary">no rate declared</Text>
                  ),
              },
              {
                title: 'Idle',
                width: 140,
                // idlePercent is a whole-number percentage from the server, not
                // a fraction in millionths — cost.percent() rounds half up
                // precisely so a figure like 78.6 reads as 79 rather than 78.
                //
                // Half the pool idle is the figure this platform exists to make
                // visible, so it gets the one colour on the page. Amber rather
                // than red: an idle GPU is usually a scheduling decision, not a
                // fault, and red would cry wolf on every quiet night.
                render: (_, p) => (
                  <span style={p.idlePercent >= 50 ? { color: '#ad6800' } : undefined}>
                    {p.idlePercent}%
                  </span>
                ),
              },
              {
                title: 'Observed',
                dataIndex: 'coveragePercent',
                width: 110,
                render: (v: number) => `${v}%`,
              },
              { title: '', render: (_, p) => <Tenants p={p} /> },
            ]}
          />
          <Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 14 }}>
            {currentPeriod()} is open and has no figure until it closes.
          </Text>
        </>
      )}
    </section>
  );
}

function Tenants({ p }: { p: CostPeriodSummary }) {
  if (!p.priced) return <Text type="secondary">—</Text>;
  return (
    <Link to={`/cost?period=${p.period}`} style={{ fontSize: 12 }}>
      who paid
    </Link>
  );
}

// The period Fleet is in: a UTC calendar month, matching pkg/cost.Period.
// Computed here rather than taken from the server because it is a property of
// the clock, not of the fleet.
function currentPeriod(): string {
  const now = new Date();
  return `${now.getUTCFullYear()}-${String(now.getUTCMonth() + 1).padStart(2, '0')}`;
}