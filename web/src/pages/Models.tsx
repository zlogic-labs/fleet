import { useState } from 'react';
import {
  Alert,
  App,
  Badge,
  Button,
  Card,
  Col,
  Descriptions,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  Popconfirm,
  Progress,
  Row,
  Select,
  Space,
  Statistic,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  CloudDownloadOutlined,
  DeleteOutlined,
  DatabaseOutlined,
  PlusOutlined,
  ReloadOutlined,
} from '@ant-design/icons';

import { models as modelsApi, pulls, storage } from '../api/control';
import { errorText } from '../api/client';
import { usePoll, humanBytes, humanAge } from '../hooks';
import type { PullJob, RegistryModel } from '../types';

const { Text, Paragraph } = Typography;

const STATE_COLOR: Record<RegistryModel['state'], string> = {
  ready: 'green',
  pulling: 'blue',
  pending: 'default',
  failed: 'red',
};

const JOB_COLOR: Record<PullJob['state'], string> = {
  done: 'green',
  running: 'blue',
  queued: 'default',
  failed: 'red',
  canceled: 'default',
};

export function Models() {
  const { message } = App.useApp();
  const [drawer, setDrawer] = useState(false);

  const storage$ = usePoll((signal) => storage.get(signal), 10000);
  const models = usePoll((signal) => modelsApi.list(signal), 5000);
  const jobs = usePoll((signal) => pulls.list(signal), 2000);

  const reachable = storage$.data !== undefined;

  return (
    <Space direction="vertical" size="middle" style={{ width: '100%' }}>
      {!reachable && !storage$.loading && (
        <Alert
          type="info"
          showIcon
          message="No control plane is answering"
          description={
            <>
              Model registry, pulls and cluster status come from <code>fleet-apiserver</code>, not
              from the gateway. Start it on port 8081, or set its address under Settings.
            </>
          }
        />
      )}

      {storage$.data && (
        <Row gutter={[16, 16]}>
          <Col xs={24} sm={12} lg={8}>
            <Card size="small">
              <Statistic
                title="Object storage"
                value={storage$.data.reachable ? 'reachable' : 'unreachable'}
                valueStyle={{
                  fontSize: 20,
                  color: storage$.data.reachable ? '#3f8600' : '#cf1322',
                }}
                prefix={<DatabaseOutlined />}
              />
              <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 8, marginBottom: 0 }}>
                {storage$.data.endpoint}
                <br />bucket <Text code>{storage$.data.bucket}</Text>
              </Paragraph>
            </Card>
          </Col>
          <Col xs={12} sm={6} lg={4}>
            <Card size="small">
              <Statistic title="Used" value={humanBytes(storage$.data.usedBytes)} />
            </Card>
          </Col>
          <Col xs={12} sm={6} lg={4}>
            <Card size="small">
              <Statistic title="Objects" value={storage$.data.objectCount} />
            </Card>
          </Col>
          <Col xs={24} sm={12} lg={8}>
            <Card size="small">
              <Statistic
                title="Registered models"
                value={models.data?.length ?? 0}
                suffix={
                  <Button
                    type="primary"
                    size="small"
                    icon={<PlusOutlined />}
                    onClick={() => setDrawer(true)}
                    disabled={!reachable}
                  >
                    Pull from Hugging Face
                  </Button>
                }
              />
            </Card>
          </Col>
        </Row>
      )}

      {storage$.data && !storage$.data.reachable && (
        <Alert
          type="error"
          showIcon
          message="Object storage is not reachable"
          description={storage$.data.message}
        />
      )}

      <Card
        size="small"
        title="Pulls"
        extra={
          <Space>
            {jobs.data?.some((j) => j.state === 'running' || j.state === 'queued') && (
              <Text type="secondary" style={{ fontSize: 12 }}>
                <Badge status="processing" /> in flight
              </Text>
            )}
            <Button size="small" icon={<ReloadOutlined />} onClick={jobs.refresh} />
          </Space>
        }
      >
        {!jobs.data || jobs.data.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="nothing downloaded yet" />
        ) : (
          <Table<PullJob>
            size="small"
            rowKey="id"
            pagination={false}
            scroll={{ x: 860 }}
            dataSource={jobs.data}
            columns={[
              {
                title: 'Model',
                dataIndex: 'model',
                ellipsis: true,
                render: (v: string, r) => (
                  <Tooltip title={r.sourceRef}>
                    <Text ellipsis style={{ maxWidth: 260 }}>
                      {v}
                    </Text>
                  </Tooltip>
                ),
              },
              { title: 'Revision', dataIndex: 'revision', width: 110, render: (v: string) => <Text code>{v}</Text> },
              {
                title: 'State',
                dataIndex: 'state',
                width: 100,
                render: (v: PullJob['state']) => <Tag color={JOB_COLOR[v]}>{v}</Tag>,
              },
              {
                title: 'Progress',
                dataIndex: 'progress',
                width: 220,
                render: (v: number, r) => (
                  <Space size={8} style={{ width: '100%' }}>
                    <Progress
                      percent={Math.round(v)}
                      size="small"
                      status={r.state === 'failed' ? 'exception' : undefined}
                      style={{ flex: 1, margin: 0 }}
                    />
                    <Text type="secondary" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                      {humanBytes(r.bytesDone)} / {humanBytes(r.bytesTotal)}
                    </Text>
                  </Space>
                ),
              },
              {
                title: 'File',
                dataIndex: 'currentFile',
                ellipsis: true,
                render: (v: string) => (
                  <Text type="secondary" style={{ fontSize: 12 }} ellipsis>
                    {v || '—'}
                  </Text>
                ),
              },
              {
                title: 'Age',
                dataIndex: 'startedAt',
                width: 80,
                render: (v: string) => humanAge(v, Date.now()),
              },
            ]}
          />
        )}
      </Card>

      <Card
        size="small"
        title="Registry"
        extra={<Button size="small" icon={<ReloadOutlined />} onClick={models.refresh} />}
      >
        {!models.data || models.data.length === 0 ? (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description="no models registered — pull one from Hugging Face above"
          />
        ) : (
          <Table<RegistryModel>
            size="small"
            rowKey="name"
            pagination={false}
            scroll={{ x: 900 }}
            dataSource={models.data}
            columns={[
              {
                title: 'Name',
                dataIndex: 'name',
                ellipsis: true,
                render: (v: string) => <Text strong>{v}</Text>,
              },
              {
                title: 'Source',
                dataIndex: 'source',
                width: 150,
                render: (v: string, r) => (
                  <Space direction="vertical" size={0}>
                    <Tag style={{ fontSize: 11 }}>{v}</Tag>
                    <Text type="secondary" style={{ fontSize: 11 }} ellipsis>
                      {r.sourceRef}
                    </Text>
                  </Space>
                ),
              },
              {
                title: 'State',
                dataIndex: 'state',
                width: 100,
                render: (v: RegistryModel['state'], r) => (
                  <Tooltip title={r.message}>
                    <Tag color={STATE_COLOR[v]}>{v}</Tag>
                  </Tooltip>
                ),
              },
              {
                title: 'Size',
                dataIndex: 'sizeBytes',
                width: 110,
                render: (v: number, r) => (
                  <Space direction="vertical" size={0}>
                    <Text style={{ fontSize: 12 }}>{v ? humanBytes(v) : '—'}</Text>
                    <Text type="secondary" style={{ fontSize: 11 }}>
                      {r.files} file{r.files === 1 ? '' : 's'}
                    </Text>
                  </Space>
                ),
              },
              { title: 'Context', dataIndex: 'contextLimit', width: 110, render: (v: number) => (v ? `${v.toLocaleString()}` : '—') },
              { title: 'Tokenizer', dataIndex: 'tokenizerId', width: 140, ellipsis: true },
              {
                title: '',
                width: 50,
                render: (_, r) => (
                  <Popconfirm
                    title="Remove from the registry?"
                    description="Stored objects are kept; only the registration is deleted."
                    onConfirm={async () => {
                      try {
                        await modelsApi.remove(r.name);
                        message.success(`${r.name} removed`);
                        models.refresh();
                      } catch (err) {
                        message.error(errorText(err));
                      }
                    }}
                  >
                    <Button size="small" type="text" danger icon={<DeleteOutlined />} />
                  </Popconfirm>
                ),
              },
            ]}
            expandable={{
              expandedRowRender: (r) => (
                <Descriptions size="small" column={1} style={{ padding: '8px 0' }}>
                  <Descriptions.Item label="Storage prefix">
                    <Text code>{r.storagePrefix}</Text>
                  </Descriptions.Item>
                  <Descriptions.Item label="Revision">
                    <Text code>{r.revision}</Text>
                  </Descriptions.Item>
                  <Descriptions.Item label="Created">{new Date(r.createdAt).toLocaleString()}</Descriptions.Item>
                  {r.message && <Descriptions.Item label="Message">{r.message}</Descriptions.Item>}
                </Descriptions>
              ),
            }}
          />
        )}
      </Card>

      <PullDrawer open={drawer} onClose={() => setDrawer(false)} onStarted={() => {
        setDrawer(false);
        jobs.refresh();
      }} />
    </Space>
  );
}

function PullDrawer({
  open,
  onClose,
  onStarted,
}: {
  open: boolean;
  onClose: () => void;
  onStarted: () => void;
}) {
  const { message } = App.useApp();
  const [form] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);

  const submit = async () => {
    let values: Record<string, unknown>;
    try {
      values = await form.validateFields();
    } catch {
      return; // Ant Design has already marked the bad fields.
    }
    setSubmitting(true);
    try {
      const ref = String(values.repo || '');
      const job = await pulls.start({
        model: ref,
        source: 'huggingface',
        sourceRef: ref,
        revision: String(values.revision || 'main'),
        contextLimit: Number(values.contextLimit || 0),
        tokenizerId: String(values.tokenizerId || ''),
        hfToken: values.hfToken ? String(values.hfToken) : undefined,
      });
      message.success(`queued pull ${job.id}`);
      form.resetFields();
      onStarted();
    } catch (err) {
      message.error(errorText(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Drawer
      title="Pull a model from Hugging Face"
      open={open}
      onClose={onClose}
      width={480}
      extra={
        <Space>
          <Button onClick={onClose}>Cancel</Button>
          <Button type="primary" icon={<CloudDownloadOutlined />} loading={submitting} onClick={submit}>
            Start
          </Button>
        </Space>
      }
    >
      <Paragraph type="secondary" style={{ fontSize: 13 }}>
        The control plane downloads the repository into object storage and registers it. A deployment
        then mounts that prefix; nothing is pulled onto a node at request time.
      </Paragraph>

      <Form form={form} layout="vertical" initialValues={{ revision: 'main', contextLimit: 32768 }}>
        <Form.Item
          name="repo"
          label="Repository"
          rules={[{ required: true, message: 'e.g. Qwen/Qwen2.5-7B-Instruct' }]}
          extra="Owner and name, as on the Hub."
        >
          <Input placeholder="Qwen/Qwen2.5-7B-Instruct" />
        </Form.Item>

        <Form.Item name="revision" label="Revision" extra="A commit hash pins the exact weights; a branch moves.">
          <Input placeholder="main" />
        </Form.Item>

        <Form.Item
          name="tokenizerId"
          label="Tokenizer"
          extra="Used for the pre-route token count. Leave blank to infer from the model name."
        >
          <Select
            allowClear
            placeholder="infer"
            options={['cl100k_base', 'o200k_base', 'p50k_base', 'r50k_base', 'gpt2'].map((v) => ({
              value: v,
              label: v,
            }))}
          />
        </Form.Item>

        <Form.Item name="contextLimit" label="Context limit" extra="0 means take it from the engine's own metadata.">
          <InputNumber min={0} step={1024} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          name="hfToken"
          label="Hugging Face token"
          extra="Only needed for gated repositories. Sent to the Hub, never stored."
        >
          <Input.Password placeholder="hf_..." autoComplete="off" />
        </Form.Item>
      </Form>
    </Drawer>
  );
}
