import { App as AntApp, Button, Form, Input, Modal, Popconfirm, Space, Table, Typography } from 'antd';
import { DeleteOutlined, KeyOutlined, PlusOutlined } from '@ant-design/icons';
import { useCallback, useState } from 'react';

import { keys as keysApi } from '../../api/control';
import type { ApiKey } from '../../types';
import { humanAge } from '../../hooks';

const { Paragraph, Text } = Typography;

/**
 * The keys of one project.
 *
 * A secret is shown exactly once, here, in response to the create that produced
 * it. Only its hash is stored, so this panel cannot show it again — which is the
 * point: a list that could re-display every key would be a list worth stealing,
 * and the copy below says so rather than leaving the operator to find out.
 */
export function Keys({ projectId, projectName, onChanged }: { projectId: string; projectName: string; onChanged: () => void }) {
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<ApiKey[]>([]);
  const [secret, setSecret] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [form] = Form.useForm();

  const reload = useCallback(async () => {
    try {
      setRows(await keysApi.list(projectId));
    } catch (err) {
      message.error(`cannot read the keys: ${err}`);
    }
  }, [projectId, message]);

  const [seen, setSeen] = useState<string>();
  if (seen !== projectId) {
    setSeen(projectId);
    void reload();
  }

  const create = useCallback(
    async (values: { label: string }) => {
      setBusy(true);
      try {
        const created = await keysApi.create(projectId, values.label);
        setSecret(created.secret);
        form.resetFields();
        await reload();
        onChanged();
      } catch (err) {
        message.error(`cannot create the key: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [projectId, form, reload, onChanged, message],
  );

  const revoke = useCallback(
    async (id: string) => {
      try {
        await keysApi.remove(id);
        await reload();
      } catch (err) {
        message.error(`cannot revoke the key: ${err}`);
      }
    },
    [reload, message],
  );

  return (
    <>
      <Modal
        open={secret !== undefined}
        title="Copy this key now"
        onCancel={() => setSecret(undefined)}
        footer={
          <Button type="primary" onClick={() => setSecret(undefined)}>
            I have copied it
          </Button>
        }
      >
        <Paragraph>
          This is the only time Fleet can show it. Only a hash is stored, so there is no way to
          recover this secret — not by an administrator, not by support.
        </Paragraph>
        <Input.TextArea value={secret} readOnly rows={3} style={{ fontFamily: 'monospace' }} />
        <Text type="secondary" style={{ fontSize: 12 }}>
          Send it over a channel that is not this browser tab.
        </Text>
      </Modal>

      <Table
        size="small"
        title={() => (
          <Space>
            <KeyOutlined />
            <span>{projectName}</span>
            <Text type="secondary" style={{ fontSize: 12 }}>
              spend is attributed to the project, not to the key
            </Text>
          </Space>
        )}
        rowKey="id"
        pagination={false}
        locale={{ emptyText: 'no keys — nobody can call this project yet' }}
        dataSource={rows}
        columns={[
          { title: 'Label', dataIndex: 'label', width: 140 },
          {
            title: 'Prefix',
            dataIndex: 'prefix',
            width: 130,
            render: (p: string) => <Text type="secondary" style={{ fontFamily: 'monospace' }}>{p}</Text>,
          },
          { title: 'Age', dataIndex: 'createdAt', width: 90, render: (v: number) => humanAge(new Date(v * 1000).toISOString(), Date.now()) },
          {
            title: '',
            width: 40,
            render: (_, k) => (
              <Popconfirm
                title="Revoke this key?"
                description="Requests using it start failing immediately. The ledger it produced stays."
                onConfirm={() => revoke(k.id)}
              >
                <Button type="text" danger size="small" icon={<DeleteOutlined />} />
              </Popconfirm>
            ),
          },
        ]}
      />

      <Form form={form} layout="inline" style={{ marginTop: 12 }} onFinish={create}>
        <Form.Item name="label" rules={[{ required: true, message: 'name the key after whatever it is for' }]}>
          <Input placeholder="label, e.g. ci" style={{ width: 180 }} />
        </Form.Item>
        <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
          Issue key
        </Button>
      </Form>
    </>
  );
}