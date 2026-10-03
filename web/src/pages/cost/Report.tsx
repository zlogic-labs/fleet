import { Alert, Card, Col, Progress, Row, Statistic, Table, Tag, Typography } from 'antd';

import { humanGpuSeconds, humanMoney, humanPct } from '../../hooks';
import type { CostReport } from '../../types';

const { Paragraph, Text } = Typography;

/** idlePercent's colour scale. Idle is the number P8 exists for, so it gets a colour. */
function idleTag(pct: number, extra?: string) {
  const color = pct > 50 ? 'red' : pct > 20 ? 'gold' : 'green';
  return (
    <Tag color={color}>
      {pct}%{extra ? ` · ${extra}` : ''}
    </Tag>
  );
}

/**
 * A closed period.
 *
 * Two sums are shown side by side because both are true: busy plus idle equals
 * the pool, and the pool is what gets allocated out. They are not two views of
 * one figure — allocating only the busy part would leave idle capacity
 * unbilled, and the operator would absorb in their own margin the exact problem
 * this platform exists to surface.
 */
export function Report({ report }: { report: CostReport }) {
  const cur = report.currency;
  return (
    <Card
      size="small"
      title={`${report.period} — closed`}
      extra={
        report.revision > 1 ? (
          <Tag color="blue">revision {report.revision}</Tag>
        ) : undefined
      }
    >
      {!report.priced && (
        <Alert
          showIcon
          type="warning"
          style={{ marginBottom: 16 }}
          message="This period is unpriced."
          description="The capacity is still reported in GPU-hours; it could not be turned into money because no rate was declared for every cluster that reported capacity."
        />
      )}
      {report.coveragePercent < 100 && (
        <Alert
          showIcon
          type="warning"
          style={{ marginBottom: 16 }}
          message={`Only ${report.coveragePercent}% of this period was observed.`}
          description="The remainder is extrapolated from what the reporter saw. A close refuses below 90% rather than issuing an invoice nobody can reconcile."
        />
      )}
      {report.amended.length > 0 && (
        <Alert
          showIcon
          type="info"
          style={{ marginBottom: 16 }}
          message={`Recomputed after the fact. ${report.amended
            .map((a) => `${a.scope} ${humanMoney(a.amount, cur)}`)
            .join(', ')} — collected by ${[...new Set(report.amended.map((a) => a.forPeriod))].join(', ')}.`}
          description="A closed invoice does not move. When a period is found to be wrong, the difference is collected by the month after it rather than rewritten into the month that was already sent."
        />
      )}

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col xs={12} md={4}>
          <Statistic title="Pool cost" value={humanMoney(report.pool, cur)} />
        </Col>
        <Col xs={12} md={4}>
          <Statistic title="Busy" value={humanMoney(report.busy, cur)} />
        </Col>
        <Col xs={12} md={4}>
          <Statistic title="Idle" value={humanMoney(report.idle, cur)} />
        </Col>
        <Col xs={12} md={4}>
          <Statistic title="Idle share" value={`${report.idlePercent}%`} />
        </Col>
        <Col xs={12} md={4}>
          <Statistic title="Capacity" value={humanGpuSeconds(report.poolGpuSeconds)} />
        </Col>
        <Col xs={12} md={4}>
          <Statistic title="Allocated" value={humanMoney(report.allocated, cur)} />
        </Col>
      </Row>

      <Progress
        percent={100 - report.idlePercent}
        format={() => 'busy'}
        strokeColor="#1677ff"
        style={{ marginBottom: 16 }}
      />

      <Paragraph type="secondary" style={{ fontSize: 12 }}>
        {report.adjustmentTotal === 0
          ? 'Allocated equals the whole pool, idle included: idle capacity is a fixed cost and has to land somewhere. Allocating only the busy part would hide it in the operator’s margin, which is the one number this platform exists to make visible.'
          : `Allocated is ${humanMoney(report.allocated, cur)} against a pool of ${humanMoney(report.pool, cur)}: the difference is ${humanMoney(report.adjustmentTotal, cur)} of corrections to earlier periods, collected by the month after them.`}
      </Paragraph>

      <Row gutter={16}>
        <Col xs={24} lg={12}>
          <Table
            size="small"
            rowKey="key"
            title={() => 'By tenant'}
            pagination={false}
            locale={{ emptyText: 'nobody used anything' }}
            dataSource={report.tenants}
            columns={[
              { title: 'Tenant', dataIndex: 'key', ellipsis: true },
              { title: 'Share', dataIndex: 'share', width: 80, align: 'right', render: humanPct },
              { title: 'GPU-hours', dataIndex: 'gpuSeconds', width: 100, align: 'right', render: humanGpuSeconds },
              // Keyed on whether there are corrections, not on their net: a
              // correction that moved a share between two tenants nets to zero
              // and is still something the person reading the invoice needs.
              ...(report.adjustments.length === 0
                ? []
                : [
                    {
                      title: 'Correction',
                      dataIndex: 'adjustment',
                      width: 110,
                      align: 'right' as const,
                      render: (v: number) =>
                        v === 0 ? (
                          <Text type="secondary">—</Text>
                        ) : (
                          <Text type={v > 0 ? 'danger' : 'success'}>{humanMoney(v, cur)}</Text>
                        ),
                    },
                  ]),
              { title: 'Allocated', dataIndex: 'amount', align: 'right', render: (v: number) => humanMoney(v, cur) },
              {
                title: 'Token ledger',
                dataIndex: 'usageMicro',
                align: 'right',
                render: (v: number) => humanMoney(v, cur),
              },
            ]}
          />
        </Col>
        <Col xs={24} lg={12}>
          <Table
            size="small"
            rowKey="name"
            title={() => 'By deployment'}
            pagination={false}
            locale={{ emptyText: 'no deployments reported' }}
            dataSource={report.deployments}
            columns={[
              { title: 'Deployment', dataIndex: 'name', ellipsis: true },
              { title: 'Held', dataIndex: 'reservedGpuSeconds', width: 96, align: 'right', render: humanGpuSeconds },
              { title: 'Used', dataIndex: 'usedGpuSeconds', width: 96, align: 'right', render: humanGpuSeconds },
              {
                title: 'Idle',
                dataIndex: 'idlePercent',
                width: 140,
                align: 'right',
                render: (v: number, d) => idleTag(v, humanGpuSeconds(d.idleGpuSeconds)),
              },
            ]}
          />
        </Col>
      </Row>
    </Card>
  );
}