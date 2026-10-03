import { Button, Card, Form, Input, InputNumber, Space, Table, Tooltip, Typography } from 'antd';
import type { FormInstance } from 'antd';
import { PlusOutlined } from '@ant-design/icons';

import { humanMoney } from '../../hooks';
import type { CostRate } from '../../types';

const { Text } = Typography;

/**
 * What a GPU-hour costs, per cluster.
 *
 * Declared rather than inferred. A cloud bill, a colocation contract and a
 * depreciated on-prem fleet have nothing in common, and Fleet is not in a
 * position to guess which one an operator has — so a period with no rate here
 * closes unpriced and says how much capacity it had instead of inventing a
 * number nobody can defend.
 */
export function RatesCard({
  rates,
  form,
  busy,
  onSubmit,
}: {
  rates: CostRate[];
  form: FormInstance<CostRate>;
  busy: boolean;
  onSubmit: (v: CostRate) => void;
}) {
  return (
    <Card
      size="small"
      title="GPU-hour rates"
      extra={
        <Text type="secondary" style={{ fontSize: 12 }}>
          declared, never inferred
        </Text>
      }
    >
      <Table
        size="small"
        rowKey="cluster"
        pagination={false}
        locale={{ emptyText: 'no rate declared; every period closes unpriced' }}
        dataSource={rates}
        columns={[
          { title: 'Cluster', dataIndex: 'cluster' },
          {
            title: 'Per GPU-hour',
            dataIndex: 'gpuHourMicro',
            align: 'right',
            render: (v: number, r: CostRate) => humanMoney(v, r.currency, 4),
          },
          { title: 'Currency', dataIndex: 'currency', width: 90 },
        ]}
      />
      <Form form={form} layout="vertical" onFinish={onSubmit}>
        <Space wrap style={{ marginTop: 12 }}>
          <Form.Item name="cluster" rules={[{ required: true, message: 'which cluster' }]}>
            <Input placeholder="cluster name" style={{ width: 150 }} />
          </Form.Item>
          <Form.Item
            name="gpuHourMicro"
            label={
              <Tooltip title="Millionths of a currency unit. 2.50 is 2500000.">
                <span>Micro per GPU-hour</span>
              </Tooltip>
            }
            rules={[{ required: true, message: 'a rate of zero is not a rate' }]}
          >
            <InputNumber min={1} style={{ width: 150 }} />
          </Form.Item>
          <Form.Item name="currency" initialValue="USD" rules={[{ required: true }]}>
            <Input placeholder="USD" style={{ width: 80 }} />
          </Form.Item>
          <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
            Declare
          </Button>
        </Space>
      </Form>
    </Card>
  );
}