import { Button, Card, Form, Input, InputNumber, Space, Table, Typography } from 'antd';
import type { FormInstance } from 'antd';
import { PlusOutlined } from '@ant-design/icons';

import { humanMoney } from '../../hooks';
import type { CostRate } from '../../types';

const { Text } = Typography;

/** What the form collects, before normalize turns currency into millionths. */
interface RateForm {
  cluster: string;
  gpuHour: number;
  currency: string;
}

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
  form: FormInstance<RateForm>;
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
        scroll={{ x: 380 }}
        locale={{ emptyText: 'no rate declared; every period closes unpriced' }}
        dataSource={rates}
        columns={[
          { title: 'Cluster', dataIndex: 'cluster' },
          {
            title: 'Per GPU-hour',
            dataIndex: 'gpuHourMicro',
            align: 'right',
            // Two decimals, not four. A GPU-hour rate is written in currency,
            // and "$2.0000" is a number nobody types; worse, it invites typing
            // "2" into the field below and meaning dollars.
            render: (v: number, r: CostRate) => humanMoney(v, r.currency, 2),
          },
          { title: 'Currency', dataIndex: 'currency', width: 90 },
        ]}
      />
      <Form
        form={form}
        layout="vertical"
        // The form speaks currency; the API speaks millionths. Converting here
        // rather than asking the operator to do it is the whole point: a rate
        // entered in the wrong unit is a millionfold error, and nothing on the
        // page would look wrong afterwards.
        onFinish={(v: RateForm) =>
          onSubmit({
            cluster: v.cluster,
            currency: v.currency,
            gpuHourMicro: Math.round((v.gpuHour ?? 0) * 1_000_000),
          })
        }
      >
        {/* Labels on all three fields, matching the table's own column headers, so
            the form reads as a row rather than as two rows interleaved. */}
        <Space wrap style={{ marginTop: 12, alignItems: 'flex-start' }}>
          <Form.Item name="cluster" label="Cluster" rules={[{ required: true, message: 'which cluster' }]}>
            <Input placeholder="k3s-dev" style={{ width: 150 }} />
          </Form.Item>
          <Form.Item
            name="gpuHour"
            label="Per GPU-hour"
            rules={[{ required: true, message: 'a rate of zero is not a rate' }]}
          >
            <InputNumber min={0.000001} step={0.25} style={{ width: 150 }} placeholder="2.00" />
          </Form.Item>
          <Form.Item name="currency" label="Currency" initialValue="USD" rules={[{ required: true }]}>
            <Input placeholder="USD" style={{ width: 80 }} />
          </Form.Item>
          {/* The label is invisible rather than absent: without it the button
              would sit on the label line instead of the input line. */}
          <Form.Item label={<span style={{ visibility: 'hidden' }}>.</span>}>
            <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
              Declare
            </Button>
          </Form.Item>
        </Space>
      </Form>
    </Card>
  );
}