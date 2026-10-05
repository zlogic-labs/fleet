import { Button, Form, Input, Table, Tag, Typography } from 'antd';
import { PlusOutlined } from '@ant-design/icons';
import { useCallback } from 'react';

import { projects as projectsApi } from '../../api/control';
import type { Project } from '../../types';
import { RowDelete } from '../../crud/RowDelete';
import { useCrud } from '../../crud/useCrud';

const { Text } = Typography;

/**
 * The projects of one tenant.
 *
 * A project id is "tenant/name" and the slash is part of it. That is not a
 * spelling preference: the gateway records the project in the ledger under
 * exactly this id, and a different spelling would be a different project as far
 * as the invoice is concerned.
 */
export function Projects({
  tenantId,
  selected,
  onSelect,
  onChanged,
}: {
  tenantId: string;
  selected?: string;
  onSelect: (projectId: string) => void;
  onChanged: () => void;
}) {
  const [form] = Form.useForm<{ name: string }>();

  const read = useCallback(() => projectsApi.list(tenantId), [tenantId]);
  const create = useCallback(
    (values: { name: string }) => projectsApi.create({ tenantId, name: values.name }),
    [tenantId],
  );

  const { rows, emptyText, busy, create: submit, remove } = useCrud<Project, { name: string }>({
    read,
    scope: tenantId,
    create,
    remove: (p) => projectsApi.remove(p.id),
    form,
    readError: 'cannot read the projects',
    empty: 'no projects — a key has to belong to one',
    createError: 'cannot create the project',
    // A project with a ledger cannot be deleted and the server says so. The
    // right answer is to stop using it, not to erase what it cost.
    removeError: 'cannot delete the project',
    onChanged,
    onRemoved: (p) => {
      if (selected === p.id) onSelect('');
    },
  });

  return (
    <>
      <Table
        size="small"
        rowKey="id"
        pagination={false}
        // The two cap columns are 330px between them; without a floor the table
        // squeezes them to one character per line rather than scrolling.
        scroll={{ x: 600 }}
        locale={{ emptyText }}
        dataSource={rows}
        onRow={(p) => ({ onClick: () => onSelect(p.id), style: { cursor: 'pointer' } })}
        rowClassName={(p) => (p.id === selected ? 'fleet-selected' : '')}
        columns={[
          { title: 'Project', dataIndex: 'name', render: (n: string) => <Tag color="blue">{n}</Tag> },
          {
            title: 'Request cap / min',
            dataIndex: 'requestLimit',
            width: 160,
            render: (v: number) => (v > 0 ? v.toLocaleString() : <Text type="secondary">inherited</Text>),
          },
          {
            title: 'Token cap / min',
            dataIndex: 'tokenLimit',
            width: 170,
            render: (v: number) => (v > 0 ? v.toLocaleString() : <Text type="secondary">inherited</Text>),
          },
          {
            title: '',
            width: 40,
            render: (_, p) => (
              <RowDelete
                row={p}
                title="Delete this project?"
                description="Refused if anything was billed under it."
                onConfirm={remove}
              />
            ),
          },
        ]}
      />

      <Form form={form} layout="inline" style={{ marginTop: 12 }} onFinish={submit}>
        <Form.Item name="name" rules={[{ required: true, message: 'a project needs a name' }]}>
          <Input placeholder="new project name" style={{ width: 200 }} />
        </Form.Item>
        <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
          Add project
        </Button>
      </Form>
    </>
  );
}
