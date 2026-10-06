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
            scroll={{ x: 560 }}
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
              // Present only when there is something in the other cost centre.
              // A permanently visible column of zeros reads as "this tenant owes
              // nothing" rather than "this fleet paid a vendor for you".
              ...(report.direct === 0
                ? []
                : [
                    {
                      title: 'Vendor',
                      dataIndex: 'direct',
                      align: 'right' as const,
                      render: (v: number) =>
                        v === 0 ? (
                          <Text type="secondary">—</Text>
                        ) : (
                          humanMoney(v, cur)
                        ),
                    },
                  ]),
              {
                title: 'Token ledger',
                dataIndex: 'usageMicro',
                align: 'right',
                render: (v: number) => humanMoney(v, cur),
              },
            ]}
          />
          {report.direct > 0 && <VendorNote report={report} />}
        </Col>
        <Col xs={24} lg={12}>
          <Table
            size="small"
            rowKey="name"
            title={() => 'By deployment'}
            pagination={false}
            scroll={{ x: 460 }}
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

/**
 * The other cost centre, named per vendor.
 *
 * Stated rather than summed into the pool, because the two are not the same
 * kind of money: the pool is already spent whether anyone used it or not, while
 * the vendor lines are what someone else actually invoiced. An operator
 * reconciling against the vendor's statement needs the line items, not a total.
 */
function VendorNote({ report }: { report: CostReport }) {
  const cur = report.currency;
  return (
    <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 10 }}>
      {humanMoney(report.direct, cur)} went to external vendors for the same traffic:{' '}
      {report.providers.map((p) => `${p.provider} ${humanMoney(p.amount, cur)}`).join(', ')}. This
      is a separate cost centre from the {humanMoney(report.allocated, cur)} pool share above —
      the pool is capacity this fleet already paid for, and these are someone
      else's invoices.
    </Paragraph>
  );
}
