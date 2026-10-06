import { Button, Card, Form, Input, InputNumber, Space, Table, Typography } from 'antd';
import type { FormInstance } from 'antd';
import { PlusOutlined } from '@ant-design/icons';

import type { PriceBook, PriceBookInput } from '../../types';

const { Text } = Typography;

/** What the form collects. Rates are whole units per million tokens. */
export interface BookForm {
  model: string;
  provider: string;
  input: number;
  output: number;
  cached: number;
}

/**
 * Token prices, per model and per provider.
 *
 * The provider column is what makes this a separate card from GPU-hour rates.
 * A book's identity is a pair: the same weights served from this fleet's own
 * pool cost a share of a fixed cost, while the identical weights from a vendor
 * cost the vendor's list price — three orders of magnitude apart, and not
 * additive. One table keyed by model alone would let whichever row was declared
 * last price both.
 *
 * An empty provider means this fleet's own capacity, which is the ordinary case,
 * so the field says that rather than requiring the word "self".
 */
export function PriceBooksCard({
  books,
  form,
  busy,
  onSubmit,
}: {
  books: PriceBook[];
  form: FormInstance<BookForm>;
  busy: boolean;
  onSubmit: (v: PriceBookInput) => void;
}) {
  return (
    <Card
      size="small"
      title="Token prices"
      extra={
        <Text type="secondary" style={{ fontSize: 12 }}>
          per million tokens, per provider
        </Text>
      }
    >
      <Table
        size="small"
        rowKey="id"
        pagination={false}
        scroll={{ x: 520 }}
        locale={{ emptyText: 'no price declared; every model is charged at the floor rate' }}
        dataSource={books}
        columns={[
          { title: 'Model', dataIndex: 'model' },
          {
            title: 'Provider',
            dataIndex: 'provider',
            width: 110,
            render: (v: string) =>
              v ? v : <Text type="secondary">this fleet</Text>,
          },
          { title: 'In', dataIndex: 'input', width: 70, align: 'right' },
          { title: 'Out', dataIndex: 'output', width: 70, align: 'right' },
          { title: 'Cached', dataIndex: 'cached', width: 78, align: 'right' },
        ]}
      />
      <Form
        form={form}
        layout="vertical"
        onFinish={(v: BookForm) =>
          onSubmit({
            model: v.model,
            // Empty string and an absent field mean the same thing here: this
            // fleet's own capacity. Sending "" rather than leaving it out keeps
            // a redeclaration from inheriting a vendor name.
            provider: (v.provider ?? '').trim().toLowerCase(),
            input: v.input,
            output: v.output,
            cached: v.cached ?? 0,
          })
        }
      >
        {/* Every field carries a label, including the two whose placeholder used
            to stand in for one. A row where some fields have a label line and
            others do not is two rows pretending to be one: the unlabelled inputs
            float up to sit with the labels while the rest hang below them. */}
        <Space wrap style={{ marginTop: 12, alignItems: 'flex-start' }}>
          <Form.Item
            name="model"
            label="Model"
            rules={[{ required: true, message: 'which model' }]}
          >
            <Input placeholder="owner/name" style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="provider" label="Provider">
            <Input placeholder="blank = this fleet" style={{ width: 190 }} />
          </Form.Item>
          <Form.Item
            name="input"
            label="in"
            rules={[{ required: true, message: 'a rate of zero is not a rate' }]}
          >
            <InputNumber min={1} style={{ width: 110 }} />
          </Form.Item>
          <Form.Item
            name="output"
            label="out"
            rules={[{ required: true, message: 'a rate of zero is not a rate' }]}
          >
            <InputNumber min={1} style={{ width: 110 }} />
          </Form.Item>
          <Form.Item name="cached" label="cached">
            <InputNumber min={0} style={{ width: 110 }} placeholder="0" />
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
      <Text type="secondary" style={{ fontSize: 12 }}>
        Rates are quota units, not currency — the ledger balances in units and
        the money figure comes from the month's cost pool. Cached is priced low
        on purpose: the margin here comes from prefix-cache hit rate, so charging
        a hit near the fresh rate taxes the behaviour the platform is trying to
        produce.
      </Text>
    </Card>
  );
}