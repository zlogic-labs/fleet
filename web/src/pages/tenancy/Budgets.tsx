import {
  Alert,
  Button,
  Form,
  InputNumber,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { PlusOutlined } from '@ant-design/icons';
import { useCallback, useMemo } from 'react';

import { budgetRules } from '../../api/control';
import type { BudgetDimension, BudgetRule } from '../../types';
import { humanWindow, windowNote } from '../../hooks';
import { RowDelete } from '../../crud/RowDelete';
import { useCrud } from '../../crud/useCrud';

const { Text } = Typography;

/**
 * The six dimensions a budget may be expressed in.
 *
 * The token split is deliberate and is the reason this is a list rather than a
 * free-text field: `tokens_fresh` counts only the prompt tokens that were not
 * served from the prefix cache, which on a deployment with good cache hit rates
 * is one or two orders of magnitude below `tokens_total`. Budgeting on total
 * prompt tokens stops a tenant whose bill is mostly cache hits.
 */
export const DIMENSIONS: { value: BudgetDimension; label: string; hint: string }[] = [
  { value: 'units', label: 'Cost', hint: 'Billed value in quota units, from the price book.' },
  { value: 'tokens_total', label: 'Tokens, all', hint: 'Prompt plus completion. The blunt cap.' },
  { value: 'tokens_fresh', label: 'Tokens, uncached', hint: 'Prompt tokens that missed the prefix cache.' },
  { value: 'tokens_input', label: 'Tokens, input', hint: 'Prompt tokens, cached or not.' },
  { value: 'tokens_output', label: 'Tokens, output', hint: 'Completion tokens only.' },
  { value: 'tokens_cached', label: 'Tokens, cached', hint: 'Prompt tokens served from cache.' },
];

const WINDOWS = ['1h', '5h', '1d', '1w', '1mo'];


/** A rule as the table renders it: the server's fields plus two labels. */
interface BudgetRow extends BudgetRule {
  key: string;
  dimensionLabel: string;
  windowLabel: string;
  windowTip: string;
}

export interface BudgetsProps {
  scopeKind: 'tenant' | 'project';
  scopeId: string;
  title: string;
  onChanged: () => void;
}

export function Budgets({ scopeKind, scopeId, title, onChanged }: BudgetsProps) {
  const [form] = Form.useForm<{ dimension: BudgetDimension; window: string; limit: number }>();

  const read = useCallback(() => budgetRules.list(scopeId), [scopeId]);

  // The window is part of the id because one dimension may be capped over
  // several windows, and removing one must not remove the others. The server
  // has one duration and cannot echo back the spelling the operator typed, so
  // the id is rebuilt from the seconds it sends -- the same function the
  // Window column renders, which is why it has to round the same way.
  const removeByRule = useCallback(
    (r: BudgetRule) =>
      budgetRules.remove(`${scopeKind}/${scopeId}/${r.dimension}/${humanWindow(r.windowSeconds)}`),
    [scopeKind, scopeId],
  );

  const { rows: rules, emptyText, busy, create: add, remove } = useCrud<BudgetRule, { dimension: BudgetDimension; window: string; limit: number }>({
    read,
    scope: scopeId,
    create: (values) => budgetRules.save({ scopeKind, scopeId, ...values }),
    remove: removeByRule,
    form,
    readError: 'cannot read the budgets',
    empty: 'no cap set — this scope is unlimited',
    createError: 'cannot set the budget',
    removeError: 'cannot remove the budget',
    onChanged,
  });

  const data = useMemo<BudgetRow[]>(
    () =>
      rules.map((r) => ({
        ...r,
        key: `${r.dimension}/${r.windowSeconds}`,
        dimensionLabel: DIMENSIONS.find((d) => d.value === r.dimension)?.label ?? r.dimension,
        windowLabel: humanWindow(r.windowSeconds),
        windowTip: windowNote(r.windowSeconds),
      })),
    [rules],
  );

  return (
    <>
      <Table<BudgetRow>
        size="small"
        title={() => (
          <Space>
            <span>{title}</span>
            <Text type="secondary" style={{ fontSize: 12 }}>
              a refusal is 402 and says when capacity comes back
            </Text>
          </Space>
        )}
        locale={{ emptyText }}
        pagination={false}
        // The Limit column has no width of its own and holds a number with a
        // unit in it, so it is the first thing to collapse when the panel is
        // narrow. A floor lets it scroll instead.
        scroll={{ x: 520 }}
        rowKey="key"
        dataSource={data}
        columns={[
          { title: 'Capped on', dataIndex: 'dimensionLabel', width: 150 },
          {
            // The window the operator typed, recovered from the seconds the
            // server sends. The delete id is built from these seconds too --
            // the server has one duration and cannot echo back a spelling.
            title: 'Window',
            dataIndex: 'windowLabel',
            width: 90,
            render: (w: string, r: BudgetRow) => (
              <Tooltip title={r.windowTip}>
                <Tag>{w}</Tag>
              </Tooltip>
            ),
          },
          {
            title: 'Limit',
            dataIndex: 'limit',
            render: (v: number, r) => `${v.toLocaleString()} ${unitOf(r.dimension)}`,
          },
          {
            title: '',
            width: 40,
            render: (_, r) => (
              <RowDelete row={r} title="Remove this cap?" description="Only this dimension and window." onConfirm={remove} />
            ),
          },
        ]}
      />

      <Form form={form} layout="inline" style={{ marginTop: 12 }} onFinish={add}>
        <Form.Item name="dimension" initialValue="units" rules={[{ required: true }]}>
          <Select style={{ width: 170 }} options={DIMENSIONS.map((d) => ({ value: d.value, label: d.label }))} />
        </Form.Item>
        <Form.Item name="window" initialValue="1mo" rules={[{ required: true }]}>
          <Select style={{ width: 100 }} options={WINDOWS.map((w) => ({ value: w, label: w }))} />
        </Form.Item>
        <Form.Item name="limit" rules={[{ required: true, message: 'a limit of zero is a deletion, not a cap' }]}>
          <InputNumber min={1} placeholder="limit" style={{ width: 160 }} />
        </Form.Item>
        <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
          Cap
        </Button>
      </Form>

      {rules.length === 0 && (
        <Alert
          style={{ marginTop: 12 }}
          type="info"
          showIcon
          message="No cap means unlimited, not zero."
          description="A scope with no rule is not refused — it is simply not rationed. Set one before handing out a key."
        />
      )}
    </>
  );
}

function unitOf(d: BudgetDimension): string {
  return d === 'units' ? 'quota units' : 'tokens';
}