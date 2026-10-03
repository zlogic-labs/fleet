import { Table, Typography } from 'antd';
import { Link } from 'react-router-dom';

import { humanMoney } from '../../hooks';
import type { CostPeriodSummary, OpenSpend } from '../../types';

const { Text } = Typography;

// What the fleet costs.
//
// Two figures, and the distinction between them is the whole point of the
// section. A closed period is an invoice: priced against the capacity the fleet
// had and the rate standing at the close, immutable once stored. The open
// period is token charges so far — real money, already in the ledger, and
// precisely what an operator wants on the first screen of the month.
//
// What the open period is not is a cost report. It has no pool and no
// allocation, because this month's capacity is not a measurement yet. Showing a
// half-month's spend beside a full month's pool would invite a utilisation
// figure that is wrong by construction.

export function Spend({
  periods,
  open,
  currency,
}: {
  periods: CostPeriodSummary[];
  open?: OpenSpend;
  currency: string;
}) {
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

      {open && open.requests > 0 && <Running open={open} currency={currency} />}

      {closed.length === 0 ? (
        // No table, because a header row over zero rows is furniture for an
        // absence. Most deployments are here for their first month.
        <Text type="secondary" style={{ fontSize: 13 }}>
          {open?.period ?? currentPeriod()} is open and prices against capacity when it closes, so
          there is no pool or idle share for it yet.
        </Text>
      ) : (
        <>
          <Table
            style={{ marginTop: 18 }}
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
            {open?.period ?? currentPeriod()} is open and prices against capacity when it closes.
          </Text>
        </>
      )}
    </section>
  );
}

// The open period, as a short row of figures rather than a table.
//
// These are token charges and they are small next to a pool — typically a few
// orders of magnitude. That is not a bug and not worth hiding: it is the reason
// the pool exists at all, and a reader who expects this number to be the bill
// learns something true from its size.
function Running({ open, currency }: { open: OpenSpend; currency: string }) {
  const cached = open.scopes.reduce((a, s) => a + s.cachedTokens, 0);
  return (
    <div style={{ marginTop: 12 }}>
      <div style={{ display: 'flex', gap: 28, alignItems: 'baseline' }}>
        <Figure
          label="Charged so far"
          value={humanMoney(open.totalsMicro, currency)}
          hint="token charges, this calendar month"
        />
        <Figure
          label="Requests"
          value={open.requests}
          hint={`${open.scopes.length} ${open.scopes.length === 1 ? 'scope' : 'scopes'}`}
        />
        <Figure
          label="From cache"
          value={cached}
          hint="prompt tokens the engine served from its prefix cache"
        />
        {open.estimated > 0 && (
          <Figure
            label="Billed on an estimate"
            value={open.estimated}
            hint="the engine reported no usage, so the reservation was charged"
          />
        )}
      </div>
      {!open.priced && (
        <Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 10 }}>
          No GPU-hour rate declared, so this is token charges only. Declare one under Cost and the
          month will also carry a pool.
        </Text>
      )}
    </div>
  );
}

function Figure({
  label,
  value,
  hint,
}: {
  label: string;
  value: string | number;
  hint?: string;
}) {
  return (
    <div>
      <div style={{ fontSize: 11, color: '#8c8c8c' }}>{label}</div>
      <div style={{ fontSize: 20 }}>{value}</div>
      {hint && (
        <div style={{ fontSize: 11, color: '#8c8c8c', maxWidth: 200, lineHeight: 1.5 }}>{hint}</div>
      )}
    </div>
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
// Only used when the server has not said; the server's answer is authoritative
// because Fleet's month is a property of its clock, not of the viewer's.
function currentPeriod(): string {
  const now = new Date();
  return `${now.getUTCFullYear()}-${String(now.getUTCMonth() + 1).padStart(2, '0')}`;
}