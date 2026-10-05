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
  SafetyCertificateOutlined,
} from '@ant-design/icons';

import { models as modelsApi, engines as enginesApi, pulls, storage } from '../api/control';
import { errorText } from '../api/client';
import { usePoll, humanBytes, humanAge } from '../hooks';
import { ControlPlaneAlert } from '../parts/control-plane';
import type { EngineProfile, PullJob, RegistryModel } from '../types';

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

const FORMAT_COLOR: Record<RegistryModel['format'], string> = {
  safetensors: 'geekblue',
  gguf: 'purple',
  unknown: 'default',
};

/**
 * The weight layout, next to the model name because it decides what can serve
 * it. An operator picking an engine needs to see this before the deployment
 * fails, not after.
 */
function FormatTag({ format }: { format: RegistryModel['format'] }) {
  if (format === 'unknown') {
    return (
      <Tooltip title="No config.json and no .gguf file were found. Nothing can load this yet.">
        <Tag color={FORMAT_COLOR.unknown} style={{ fontSize: 11 }}>
          no weights
        </Tag>
      </Tooltip>
    );
  }
  return (
    <Tag color={FORMAT_COLOR[format]} style={{ fontSize: 11 }}>
      {format}
    </Tag>
  );
}

export function Models() {
  const { message } = App.useApp();
  const [drawer, setDrawer] = useState(false);

  const storage$ = usePoll((signal) => storage.get(signal), 10000);
  const models = usePoll((signal) => modelsApi.list(signal), 5000);
  const jobs = usePoll((signal) => pulls.list(signal), 2000);
  const engines$ = usePoll((signal) => enginesApi.list(signal), 30000);

  const reachable = storage$.data !== undefined;

  /**
   * Ask the control plane whether the stored objects satisfy an engine.
   *
   * The engine is chosen from the server's own profile list rather than
   * hard-coded here, so a new engine in the control plane becomes checkable
   * without a frontend change. Choosing the first compatible engine is not a
   * guess: the format decides it, and the control plane has already said
   * which engines that is.
   */
  const check = async (m: RegistryModel) => {
    const target = engines$.data?.find((e) => e.knownModels.includes(m.format));
    if (!target) {
      message.warning(`no registered engine can load ${m.format} weights`);
      return;
    }
    try {
      const r = await modelsApi.usableBy(m.name, target.name);
      if (r.usable) {
        message.success(`${m.name}: ${r.objects} objects satisfy ${r.engine}`);
      } else {
        message.error(`${m.name} is incomplete for ${r.engine}: ${r.missing.join(', ')}`);
      }
    } catch (err) {
      message.error(errorText(err));
    }
  };

  return (
    <Space direction="vertical" size="middle" style={{ width: '100%' }}>
      {!reachable && !storage$.loading && (
        <ControlPlaneAlert
          error={storage$.error ?? models.error}
          hint={
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
                    {/* The wire format is a fraction in [0,1]; the component
                        wants 0-100. Passing it through unchanged renders a
                        finished download as 1%. */}
                    <Progress
                      percent={Math.round(v * 100)}
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
                render: (v: string, r) => (
                  <Space direction="vertical" size={0}>
                    <Text strong>{v}</Text>
                    <FormatTag format={r.format} />
                  </Space>
                ),
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
              {
                title: 'Tokenizer',
                dataIndex: 'tokenizerId',
                width: 140,
                ellipsis: true,
                // GGUF carries its tokenizer inside the weights file, so an
                // empty id is the correct value rather than a missing one.
                render: (v: string, r) => v || (r.format === 'gguf' ? <Text type="secondary">embedded</Text> : '—'),
              },
              {
                title: '',
                width: 96,
                render: (_, r) => (
                  <Space size={0}>
                    <Tooltip title="Check the stored objects against an engine that can load this format">
                      <Button
                        size="small"
                        type="text"
                        icon={<SafetyCertificateOutlined />}
                        disabled={r.state !== 'ready' || r.format === 'unknown'}
                        onClick={() => void check(r)}
                      />
                    </Tooltip>
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
                  </Space>
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

      <EngineCard engines={engines$.data} />

      <PullDrawer open={drawer} onClose={() => setDrawer(false)} onStarted={() => {
        setDrawer(false);
        jobs.refresh();
      }} />
    </Space>
  );
}

/**
 * The engine catalogue, straight from the control plane.
 *
 * It is here because the two questions an operator asks about a model — what
 * can load this, and what will it refuse — are both answered by these
 * profiles. Hiding that behind a failed deployment would make the platform look
 * unpredictable when it is merely explicit.
 */
function EngineCard({ engines }: { engines?: EngineProfile[] }) {
  if (!engines || engines.length === 0) return null;
  return (
    <Card size="small" title="Engines">
      <Table<EngineProfile>
        size="small"
        rowKey="name"
        pagination={false}
        scroll={{ x: 820 }}
        dataSource={engines}
        columns={[
          {
            title: 'Engine',
            dataIndex: 'name',
            width: 120,
            render: (v: string) => <Text strong>{v}</Text>,
          },
          {
            title: 'Loads',
            dataIndex: 'format',
            width: 120,
            render: (v: string) => <Tag color={v === 'gguf' ? 'purple' : 'geekblue'}>{v}</Tag>,
          },
          {
            title: 'Hardware',
            dataIndex: 'minComputeLabel',
            width: 210,
            render: (v: string) =>
              v ? <Text style={{ fontSize: 12 }}>{v}</Text> : <Text type="secondary">CPU is fine</Text>,
          },
          {
            title: 'Autoscaling',
            dataIndex: 'metrics',
            width: 120,
            render: (v: boolean) =>
              v ? (
                <Tag color="green">metrics</Tag>
              ) : (
                <Tooltip title="No usable metric set, so replicas are set by hand.">
                  <Tag>manual</Tag>
                </Tooltip>
              ),
          },
          {
            title: 'Exact token counts',
            dataIndex: 'tokenize',
            width: 150,
            render: (v: boolean) => (
              <Tooltip title={v ? 'Serves /tokenize, so prompt counts are reconciled against the engine.' : 'No /tokenize: the gateway counts locally and estimates.'}>
                <Tag color={v ? 'green' : 'default'}>{v ? 'engine /tokenize' : 'local only'}</Tag>
              </Tooltip>
            ),
          },
          {
            title: 'Notes',
            dataIndex: 'notes',
            render: (v: string) => (
              <Text type="secondary" style={{ fontSize: 12 }}>
                {v}
              </Text>
            ),
          },
        ]}
      />
    </Card>
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
  // The engine list is a server fact, so the dropdown is built from the same
  // endpoint the catalogue card uses. A hard-coded list here would be a second
  // place to forget an engine.
  const engines$ = usePoll((signal) => enginesApi.list(signal), 30000);
  const engineOptions = (engines$.data ?? []).map((e) => ({
    value: e.name,
    label: `${e.name} — ${e.format}`,
  }));

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
        engine: values.engine ? String(values.engine) : undefined,
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
          name="engine"
          label="Intended engine"
          extra="Optional, and does not change what is downloaded: the format is read from the repository's own files. Naming one lets the console check the result against it."
        >
          <Select allowClear placeholder="decide later" options={engineOptions} />
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
