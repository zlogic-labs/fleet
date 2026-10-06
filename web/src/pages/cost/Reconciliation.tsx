import { Alert, Card, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import { humanMoney } from '../../hooks';
import type { ReconciliationTally, UsageReconciliation } from '../../types';

const { Text } = Typography;

/**
 * Does the store a report is read from still hold what the books hold.
 *
 * A different question from the metering audit above, with a different remedy.
 * That one asks whether the gateway measured a request the way the engine did;
 * this asks whether the replica has the rows at all. A dropped batch under load,
 * a restore from an older backup, a row the backfill wrote twice — each leaves
 * two stores that both look healthy and disagree, and every report reads from
 * one of them and therefore agrees with itself.
 *
 * Read-only, and it has to be. The ledger is authoritative, so a difference here
 * is a fact about the replica rather than a correction to a month whose invoice
 * has already gone out.
 */
export function Reconciliation({ report }: { report?: UsageReconciliation }) {
  if (!report) {
    return (
      <Card title="Ledger vs replica" extra="read-only" style={{ marginTop: 16 }}>
        <Text type="secondary">Nothing to compare yet.</Text>
      </Card>
    );
  }

  // Built from the server's own list rather than by comparing the two tallies
  // here: which figures count as differing is a policy the report states, and
  // deriving it a second time in the browser is how the two come to disagree.
  const differing = new Set(report.fields.map((f) => f.field));

  const rows: FigureRow[] = FIGURES.map((f) => ({
    key: f.key,
    label: f.label,
    money: f.money,
    ledger: report.ledger[f.key],
    replica: report.replica[f.key],
    differs: differing.has(f.key),
  }));

  const columns: ColumnsType<FigureRow> = [
    { title: 'Figure', dataIndex: 'label' },
    {
      title: 'Ledger',
      dataIndex: 'ledger',
      align: 'right',
      render: (v: number, row) => figure(v, row.money),
    },
    // Absent rather than blank when there is no replica: a column of dashes
    // reads as a comparison that found nothing, which is the one thing it is
    // not.
    ...(report.available
      ? [
          {
            title: 'Replica',
            dataIndex: 'replica',
            align: 'right' as const,
            render: (v: number, row: FigureRow) =>
              row.differs ? <Text type="danger">{figure(v, row.money)}</Text> : figure(v, row.money),
          },
        ]
      : []),
  ];

  return (
    <Card
      title="Ledger vs replica"
      extra={
        <Text type="secondary" style={{ fontSize: 12 }}>
          {report.period} · read-only
        </Text>
      }
      style={{ marginTop: 16 }}
    >
      {!report.available && (
        <Alert
          showIcon
          type="info"
          style={{ marginBottom: 16 }}
          message="There is nothing to compare against."
          description={report.note}
        />
      )}

      {report.available && !report.agreed && (
        <Alert
          showIcon
          type="warning"
          style={{ marginBottom: 16 }}
          message="The reporting copy does not match the books."
          description={
            <>
              Nothing here changes a figure: the ledger is what a closed period was priced from, and a
              difference is a fact about the replica. What it means depends on which way the rows moved — a
              replica short of rows lost writes, and one with extra rows wrote some twice.
            </>
          }
        />
      )}

      {report.available && report.agreed && (
        <Text type="secondary" style={{ fontSize: 12 }}>
          The replica holds the same rows with the same figures. Rows with no tenant are counted in the
          totals but cannot appear under a key below, which is why the two are measured separately.
        </Text>
      )}

      {report.lagSeconds > 0 && (
        <div style={{ marginTop: 8 }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {report.period} has not ended, so the comparison stops {humanLag(report.lagSeconds)} short of
            now: the mirror is written through a queue that flushes every few seconds, and comparing up to
            this instant would report the last few seconds as missing rows every time.
          </Text>
        </div>
      )}

      <Table
        rowKey="key"
        size="small"
        style={{ marginTop: 16 }}
        pagination={false}
        dataSource={rows}
        columns={columns}
        scroll={{ x: 420 }}
      />

      {report.groups.length > 0 && (
        <>
          <Table
            rowKey="key"
            size="small"
            style={{ marginTop: 16 }}
            pagination={false}
            dataSource={report.groups}
            scroll={{ x: 820 }}
            columns={[
              { title: 'Tenant / model', dataIndex: 'key', ellipsis: true },
              {
                title: 'Rows (ledger)',
                width: 110,
                align: 'right',
                render: (_, g) => g.ledger.records.toLocaleString(),
              },
              {
                title: 'Rows (replica)',
                width: 110,
                align: 'right',
                render: (_, g) => <Text type="danger">{g.replica.records.toLocaleString()}</Text>,
              },
              {
                title: 'Charges (ledger)',
                width: 130,
                align: 'right',
                render: (_, g) => humanMoney(g.ledger.amountMicro),
              },
              {
                title: 'Charges (replica)',
                width: 130,
                align: 'right',
                render: (_, g) => <Text type="danger">{humanMoney(g.replica.amountMicro)}</Text>,
              },
              { title: 'What moved', dataIndex: 'reason', ellipsis: true },
            ]}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            {report.omitted > 0
              ? `${report.omitted} more groups disagree and are not listed; the report names at most ${report.groupLimit}.`
              : `At most ${report.groupLimit} groups are named; all of them fit.`}
          </Text>
        </>
      )}
    </Card>
  );
}

interface FigureRow {
  key: keyof ReconciliationTally;
  label: string;
  money: boolean;
  ledger: number;
  replica: number;
  differs: boolean;
}

const FIGURES: { key: keyof ReconciliationTally; label: string; money: boolean }[] = [
  { key: 'records', label: 'Rows', money: false },
  { key: 'promptTokens', label: 'Prompt tokens', money: false },
  { key: 'completionTokens', label: 'Completion tokens', money: false },
  { key: 'cachedTokens', label: 'Cached tokens', money: false },
  { key: 'amountMicro', label: 'Token charges', money: true },
];

function figure(v: number, money: boolean): string {
  return money ? humanMoney(v) : v.toLocaleString();
}

/** The lag as a duration a person would say out loud. */
function humanLag(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  return `${minutes} minute${minutes === 1 ? '' : 's'}`;
}
