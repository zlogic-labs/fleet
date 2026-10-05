import { Alert, Card, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import type { UsageAgreement } from '../../types';
import { Measures } from '../overview/parts';

const { Text } = Typography;

/**
 * Fleet's metering, audited against itself.
 *
 * The engine counts its own output; when it does not, the gateway counts the
 * text it forwarded. Those two are produced by different machinery, and when
 * they drift apart the only symptom is an invoice that is a few percent wrong
 * in a direction nobody can explain. Nothing else in the console can show
 * that, because every other figure is derived from one of the two and so
 * agrees with itself by construction.
 *
 * Read-only, and deliberately not a chart: the interesting case is one endpoint
 * among several disagreeing, which is a row in a table rather than a point on a
 * line.
 */
export function Agreement({ report }: { report?: UsageAgreement }) {
  if (!report) {
    return (
      <Card title="Metering agreement" extra="audit">
        <Text type="secondary">Nothing to compare yet.</Text>
      </Card>
    );
  }

  const { sources, keys, faults, minSamples, tolerancePercent } = report;
  const measured = (sources.engine ?? 0) + (sources.counted ?? 0);
  const guessed = sources.reserved ?? 0;

  const columns: ColumnsType<UsageAgreement['keys'][number]> = [
    { title: 'Endpoint', dataIndex: 'endpoint' },
    {
      title: 'Engine said',
      dataIndex: 'engineRows',
      align: 'right',
      render: (n: number) => `${n}`,
    },
    {
      title: 'Gateway counted',
      dataIndex: 'countedRows',
      align: 'right',
      render: (n: number) => `${n}`,
    },
    {
      title: 'Divergence',
      dataIndex: 'percent',
      align: 'right',
      render: (p: number, row) =>
        row.countedRows < minSamples || row.engineRows < minSamples ? (
          <Text type="secondary">not enough to compare</Text>
        ) : (
          `${p > 0 ? '+' : ''}${p}%`
        ),
    },
    {
      title: '',
      dataIndex: 'fault',
      render: (fault: boolean, row) => {
        if (fault) return <Tag color="red">disagrees</Tag>;
        if (row.truncatedRows > 0) return <Tag>a capped answer</Tag>;
        return <Text type="secondary">agrees</Text>;
      },
    },
  ];

  return (
    <Card
      title="Metering agreement"
      extra={<Text type="secondary">audit · tolerance {tolerancePercent}%</Text>}
      style={{ marginTop: 16 }}
    >
      <Measures
        items={[
          { label: 'reported by the engine', value: sources.engine ?? 0, hint: 'charged exactly' },
          { label: 'counted by Fleet', value: sources.counted ?? 0, hint: 'the text we forwarded' },
          { label: 'billed at the ceiling', value: guessed, hint: 'no usage, no text to count' },
          { label: 'requests compared', value: measured, hint: 'this calendar month' },
        ]}
      />

      {faults > 0 ? (
        <Alert
          showIcon
          type="warning"
          style={{ marginBottom: 16 }}
          message={`${faults} endpoint${faults === 1 ? '' : 's'} disagree${faults === 1 ? 's' : ''} with the engine by more than ${tolerancePercent}%`}
          description={
            <>
              One side is wrong. The usual cause is a tokenizer that does not match the model, so set the
              model's tokenizer id under Settings rather than leaving it to be guessed.
            </>
          }
        />
      ) : (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {`No endpoint diverges by more than ${tolerancePercent}%. A key needs at least ${minSamples} requests of each kind before it can be compared at all, so a small one reads as absent rather than as agreement.`}
        </Text>
      )}

      {keys.length > 0 && (
        <Table
          rowKey="endpoint"
          size="small"
          style={{ marginTop: 16 }}
          dataSource={keys}
          columns={columns}
          pagination={false}
          scroll={{ x: 620 }}
        />
      )}
    </Card>
  );
}