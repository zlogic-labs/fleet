import { Button, Form, Input, Modal, Space, Table, Typography } from 'antd';
import { KeyOutlined, PlusOutlined } from '@ant-design/icons';
import { useCallback, useState } from 'react';

import { keys as keysApi } from '../../api/control';
import type { ApiKey } from '../../types';
import { humanAge } from '../../hooks';
import { RowDelete } from '../../crud/RowDelete';
import { useCrud } from '../../crud/useCrud';

const { Paragraph, Text } = Typography;

/**
 * The keys of one project.
 *
 * A secret is shown exactly once, here, in response to the create that produced
 * it. Only its hash is stored, so this panel cannot show it again — which is
 * the point: a list that could re-display every key would be a list worth
 * stealing, and the copy below says so rather than leaving the operator to find
 * out.
 */
export function Keys({ projectId, projectName, onChanged }: { projectId: string; projectName: string; onChanged: () => void }) {
  const [form] = Form.useForm<{ label: string }>();
  const [secret, setSecret] = useState<string>();

  const read = useCallback(() => keysApi.list(projectId), [projectId]);

  const { rows, emptyText, busy, create, remove } = useCrud<ApiKey, { label: string }>({
    read,
    scope: projectId,
    // The created row carries the one secret Fleet will ever show, so it cannot
    // be handed to the generic reload path: it has to be captured here.
    create: async (values) => {
      const created = await keysApi.create(projectId, values.label);
      setSecret(created.secret);
    },
    remove: (k) => keysApi.remove(k.id),
    form,
    readError: 'cannot read the keys',
    empty: 'no keys — nobody can call this project yet',
    createError: 'cannot create the key',
    removeError: 'cannot revoke the key',
    onChanged,
  });

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
        scroll={{ x: 460 }}
        locale={{ emptyText }}
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
              <RowDelete
                row={k}
                title="Revoke this key?"
                description="Requests using it start failing immediately. The ledger it produced stays."
                onConfirm={remove}
              />
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
