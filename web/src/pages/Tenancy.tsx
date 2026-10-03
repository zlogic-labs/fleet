import { Alert, App as AntApp, Button, Card, Col, Form, Input, InputNumber, Popconfirm, Row, Space, Table, Tag, Typography } from 'antd';
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';
import { useCallback, useState } from 'react';

import { tenants as tenantsApi } from '../api/control';
import { usePoll } from '../hooks';
import type { Tenant } from '../types';
import { Budgets } from './tenancy/Budgets';
import { Keys } from './tenancy/Keys';
import { Projects } from './tenancy/Projects';

const { Text } = Typography;

/**
 * Tenancy: who may call this platform, and what each of them may spend.
 *
 * The shape of the page follows the shape of the decision. An operator picks a
 * tenant, then a project inside it, and everything they can configure belongs
 * to one of the two: a cap on the tenant is an envelope, a cap on the project
 * is a slice of it. Keys live under the project because a key is a credential
 * for a project, not a way to spend against a tenant directly.
 */
export function Tenancy() {
  const { message } = AntApp.useApp();
  const tenants$ = usePoll((signal) => tenantsApi.list(signal), 15000);
  const [picked, setPicked] = useState<string>();
  const [project, setProject] = useState('');
  const [busy, setBusy] = useState(false);
  const [form] = Form.useForm();

  const refresh = tenants$.refresh;
  const reload = useCallback(() => refresh(), [refresh]);

  const tenants = tenants$.data ?? [];
  // Selection follows the data: a poll can remove the tenant that was picked,
  // and then the page would render detail for a scope that no longer exists.
  const current = tenants.find((t) => t.id === picked) ?? tenants[0];

  const create = useCallback(
    async (values: { id: string; name: string; requestLimit: number; tokenLimit: number }) => {
      setBusy(true);
      try {
        await tenantsApi.create({ ...values, active: true });
        form.resetFields();
        reload();
      } catch (err) {
        message.error(`cannot create the tenant: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [form, reload, message],
  );

  const remove = useCallback(
    async (id: string) => {
      try {
        await tenantsApi.remove(id);
        setPicked('');
        setProject('');
        reload();
      } catch (err) {
        message.error(`cannot delete the tenant: ${err}`);
      }
    },
    [reload, message],
  );

  const projectName = project.split('/').slice(1).join('/');

  return (
    <>
      {tenants$.error && tenants$.data === undefined && (
        <Alert
          showIcon
          type="info"
          style={{ marginBottom: 16 }}
          message="No control plane is answering"
          description="Tenants, projects, keys and budgets live in fleet-apiserver, not in the gateway. Start it on port 8081, or set its address under Settings."
        />
      )}

      <Row gutter={16}>
        <Col xs={24} lg={9}>
          <Card
            size="small"
            title="Tenants"
            extra={<Text type="secondary" style={{ fontSize: 12 }}>a billing relationship and a contract</Text>}
          >
            <Table
              size="small"
              rowKey="id"
              pagination={false}
              locale={{ emptyText: 'none yet' }}
              dataSource={tenants}
              onRow={(t) => ({ onClick: () => setPicked(t.id), style: { cursor: 'pointer' } })}
              rowClassName={(t) => (t.id === current?.id ? 'fleet-selected' : '')}
              columns={[
                { title: 'Tenant', dataIndex: 'id', render: (id: string, t: Tenant) => (
                  <Space size={4}>
                    <span>{id}</span>
                    {!t.active && <Tag color="default">inactive</Tag>}
                  </Space>
                ) },
                {
                  title: '',
                  width: 40,
                  render: (_, t) => (
                    <Popconfirm
                      title="Delete this tenant?"
                      description="Refused if anything was billed under it. An inactive tenant is the honest way to stop traffic."
                      onConfirm={() => remove(t.id)}
                    >
                      <Button type="text" danger size="small" icon={<DeleteOutlined />} onClick={(e) => e.stopPropagation()} />
                    </Popconfirm>
                  ),
                },
              ]}
            />
            <Form form={form} layout="vertical" style={{ marginTop: 12 }} onFinish={create}>
              <Form.Item name="id" label="Id" rules={[{ required: true }]}>
                <Input placeholder="acme" />
              </Form.Item>
              <Form.Item name="name" label="Name" rules={[{ required: true }]}>
                <Input placeholder="Acme Corp" />
              </Form.Item>
              <Space>
                <Form.Item name="requestLimit" label="Requests / min" tooltip="0 inherits the gateway default">
                  <InputNumber min={0} placeholder="0" style={{ width: 130 }} />
                </Form.Item>
                <Form.Item name="tokenLimit" label="Tokens / min" tooltip="0 inherits the gateway default">
                  <InputNumber min={0} placeholder="0" style={{ width: 130 }} />
                </Form.Item>
              </Space>
              <Button type="primary" htmlType="submit" icon={<PlusOutlined />} loading={busy}>
                Add tenant
              </Button>
            </Form>
          </Card>
        </Col>

        <Col xs={24} lg={15}>
          {!current ? (
            <Card size="small">
              <Text type="secondary">Add a tenant to begin.</Text>
            </Card>
          ) : (
            <Space direction="vertical" size={16} style={{ width: '100%' }}>
              <Card size="small" title={`${current.id} — the envelope`}>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  A project cap is a slice of this, not an addition to it. A tenant cannot spend
                  more than its envelope by having more projects.
                </Text>
                <Table
                  size="small"
                  pagination={false}
                  rowKey="dimension"
                  style={{ marginTop: 12 }}
                  dataSource={[
                    { dimension: 'Request limit', value: current.requestLimit, unit: 'per minute' },
                    { dimension: 'Token limit', value: current.tokenLimit, unit: 'per minute' },
                  ]}
                  columns={[
                    { title: 'Ceiling', dataIndex: 'dimension' },
                    {
                      title: 'Value',
                      dataIndex: 'value',
                      align: 'right' as const,
                      render: (v: number) => (v > 0 ? v.toLocaleString() : <Text type="secondary">gateway default</Text>),
                    },
                    { title: '', dataIndex: 'unit', render: (u: string) => <Text type="secondary">{u}</Text> },
                  ]}
                />
              </Card>

              <Card size="small" title={`${current.id} — projects`}>
                <Projects tenantId={current.id} selected={project} onSelect={setProject} onChanged={reload} />
              </Card>

              {project && (
                <>
                  <Card size="small" title="Keys">
                    <Keys projectId={project} projectName={projectName} onChanged={reload} />
                  </Card>
                  <Card size="small">
                    <Budgets
                      scopeKind="project"
                      scopeId={project}
                      title={`${projectName} — budgets`}
                      onChanged={reload}
                    />
                  </Card>
                </>
              )}

              <Card size="small">
                <Budgets
                  scopeKind="tenant"
                  scopeId={current.id}
                  title={`${current.id} — tenant budgets`}
                  onChanged={reload}
                />
              </Card>
            </Space>
          )}
        </Col>
      </Row>
    </>
  );
}