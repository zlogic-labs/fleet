import { App as AntApp, Button, Form, Input, Popconfirm, Table, Tag, Typography } from 'antd';
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';
import { useCallback, useState } from 'react';

import { projects as projectsApi } from '../../api/control';
import type { Project } from '../../types';

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
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<Project[]>([]);
  const [busy, setBusy] = useState(false);
  const [form] = Form.useForm();

  const reload = useCallback(async () => {
    try {
      setRows(await projectsApi.list(tenantId));
    } catch (err) {
      message.error(`cannot read the projects: ${err}`);
    }
  }, [tenantId, message]);

  const [seen, setSeen] = useState<string>();
  if (seen !== tenantId) {
    setSeen(tenantId);
    void reload();
  }

  const create = useCallback(
    async (values: { name: string }) => {
      setBusy(true);
      try {
        await projectsApi.create({ tenantId, name: values.name });
        form.resetFields();
        await reload();
        onChanged();
      } catch (err) {
        message.error(`cannot create the project: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [tenantId, form, reload, onChanged, message],
  );

  const remove = useCallback(
    async (id: string) => {
      try {
        await projectsApi.remove(id);
        await reload();
        if (selected === id) onSelect('');
      } catch (err) {
        // A project with a ledger cannot be deleted, and the server says so.
        // The right answer is to stop using it, not to erase what it cost.
        message.error(`cannot delete the project: ${err}`);
      }
    },
    [reload, selected, onSelect, message],
  );

  return (
    <>
      <Table
        size="small"
        rowKey="id"
        pagination={false}
        locale={{ emptyText: 'no projects — a key has to belong to one' }}
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
              <Popconfirm
                title="Delete this project?"
                description="Refused if anything was billed under it."
                onConfirm={() => remove(p.id)}
              >
                <Button type="text" danger size="small" icon={<DeleteOutlined />} onClick={(e) => e.stopPropagation()} />
              </Popconfirm>
            ),
          },
        ]}
      />

      <Form form={form} layout="inline" style={{ marginTop: 12 }} onFinish={create}>
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