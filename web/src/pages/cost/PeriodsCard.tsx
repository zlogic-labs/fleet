import { Button, Card, Form, Input, Popconfirm, Space, Table, Tag, Typography } from 'antd';
import type { FormInstance } from 'antd';
import { PlusOutlined } from '@ant-design/icons';

import { humanMoney } from '../../hooks';
import type { CostPeriodSummary } from '../../types';

const { Text } = Typography;

/**
 * Closed periods, and the control that creates one.
 *
 * Closing is a separate action from closing the page because it is an
 * irreversible event: the pool is priced and stored, and a second close of the
 * same month answers 409 rather than silently replacing the invoice. The
 * confirmation says so, and the period is validated as YYYY-MM before it is
 * sent, because "September" is not a period and neither is "2026".
 */
export function PeriodsCard({
  periods,
  open,
  form,
  busy,
  onPick,
  onSubmit,
}: {
  periods: CostPeriodSummary[];
  open?: string;
  form: FormInstance<{ period: string }>;
  busy: boolean;
  onPick: (period: string) => void;
  onSubmit: (v: { period: string }) => void;
}) {
  return (
    <Card
      size="small"
      title="Periods"
      extra={
        <Text type="secondary" style={{ fontSize: 12 }}>
          a closed period is an invoice
        </Text>
      }
    >
      <Table
        size="small"
        rowKey="period"
        pagination={false}
        locale={{ emptyText: 'nothing closed yet' }}
        dataSource={periods}
        onRow={(p) => ({ onClick: () => onPick(p.period), style: { cursor: 'pointer' } })}
        rowClassName={(p) => (p.period === open ? 'fleet-selected' : '')}
        columns={[
          { title: 'Period', dataIndex: 'period' },
          {
            title: 'Pool',
            dataIndex: 'pool',
            align: 'right',
            render: (v: number, p: CostPeriodSummary) => humanMoney(v, p.currency),
          },
          {
            title: 'Idle',
            dataIndex: 'idlePercent',
            align: 'right',
            render: (v: number) => (
              <Tag color={v > 50 ? 'red' : v > 20 ? 'gold' : 'green'}>{v}%</Tag>
            ),
          },
          {
            title: 'Coverage',
            dataIndex: 'coveragePercent',
            align: 'right',
            render: (v: number) => `${v}%`,
          },
        ]}
      />
      <Form form={form} layout="vertical" onFinish={onSubmit} style={{ marginTop: 12 }}>
        <Space>
          <Form.Item
            name="period"
            rules={[
              { required: true, message: 'YYYY-MM' },
              {
                pattern: /^\d{4}-(0[1-9]|1[0-2])$/,
                message: 'a period is a calendar month, YYYY-MM',
              },
            ]}
          >
            <Input placeholder={previousPeriod()} style={{ width: 130 }} />
          </Form.Item>
          <Popconfirm
            title="Close this period?"
            description="It is billed out and stored. It cannot be reopened."
            onConfirm={() => form.submit()}
          >
            <Button type="primary" icon={<PlusOutlined />} loading={busy}>
              Close period
            </Button>
          </Popconfirm>
        </Space>
      </Form>
      <Text type="secondary" style={{ fontSize: 12 }}>
        Pick a row to see its allocation.
      </Text>
    </Card>
  );
}

// The month before this one, as the placeholder.
//
// A literal placeholder goes stale the moment it is written, and a stale one
// is worse than none: it suggested a period that had already closed the first
// time the page was loaded in a later month.
function previousPeriod(): string {
  const now = new Date();
  const first = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1);
  const back = new Date(first - 24 * 60 * 60 * 1000);
  return `${back.getUTCFullYear()}-${String(back.getUTCMonth() + 1).padStart(2, '0')}`;
}
