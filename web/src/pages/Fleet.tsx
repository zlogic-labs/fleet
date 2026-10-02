import {
  Alert,
  Badge,
  Card,
  Col,
  Descriptions,
  Empty,
  Progress,
  Row,
  Space,
  Statistic,
  Table,
  Tag,
  Typography,
} from 'antd';
import { InfoCircleOutlined } from '@ant-design/icons';

import { usePoll } from '../hooks';
import { fleetStatus } from '../api/gateway';
import { CAPABILITIES, type EndpointStatus, type RecentSample } from '../types';

const { Text, Paragraph } = Typography;

export function Fleet() {
  const { data, error, loading } = usePoll((signal) => fleetStatus(signal), 5000);

  if (error) {
    return (
      <Alert
        type="warning"
        showIcon
        message="The gateway did not answer /fleet/status"
        description={error.message}
      />
    );
  }
  if (loading && !data) return <Card loading />;
  if (!data) {
    return (
      <Empty description="The gateway returned no status. Is it running?" />
    );
  }

  const granted = new Set(data.capabilities);
  const names = Object.keys(CAPABILITIES);
  const missing = names.filter((n) => !granted.has(n));

  const totalTok = data.recent.reduce((a, s) => a + s.completionTokens, 0);
  const estimated = data.recent.filter((s) => s.estimated).length;
  const avgTtft = data.recent.length
    ? Math.round(data.recent.reduce((a, s) => a + s.ttftMs, 0) / data.recent.length)
    : 0;

  return (
    <Space direction="vertical" size="middle" style={{ width: '100%' }}>
      <Row gutter={[16, 16]}>
        <Col xs={24} sm={12} lg={6}>
          <Card size="small">
            <Statistic title="Endpoints" value={data.endpoints.length} />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card size="small">
            <Statistic title="Healthy" value={data.endpoints.filter((e) => e.healthy).length}
              suffix={`/ ${data.endpoints.length}`} />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card size="small">
            <Statistic title="Avg time to first token" value={avgTtft} suffix="ms" />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card size="small">
            <Statistic
              title="Completion tokens (recent)"
              value={totalTok}
              suffix={estimated > 0 ? `${estimated} estimated` : ''}
            />
          </Card>
        </Col>
      </Row>

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={12}>
          <Card size="small" title="Endpoints" extra={<Text type="secondary">from /fleet/status</Text>}>
            {data.endpoints.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="no endpoints registered" />
            ) : (
              <Space direction="vertical" size="middle" style={{ width: '100%' }}>
                {data.endpoints.map((ep) => (
                  <EndpointRow key={`${ep.model}@${ep.baseUrl}`} ep={ep} />
                ))}
              </Space>
            )}
          </Card>
        </Col>

        <Col xs={24} xl={12}>
          <Card
            size="small"
            title={
              <Space>
                Entitlements
                <Tag color={data.edition === 'enterprise' ? 'green' : 'default'}>
                  {data.edition}
                </Tag>
              </Space>
            }
          >
            <Table
              size="small"
              rowKey="cap"
              pagination={false}
              showHeader={false}
              dataSource={names.map((n) => ({ cap: n, on: granted.has(n) }))}
              columns={[
                {
                  title: 'Capability',
                  dataIndex: 'cap',
                  render: (v: string) => CAPABILITIES[v] ?? v,
                },
                {
                  title: '',
                  dataIndex: 'on',
                  align: 'right',
                  render: (on: boolean) =>
                    on ? <Tag color="green">granted</Tag> : <Tag>community</Tag>,
                },
              ]}
            />
            {missing.length > 0 && data.edition !== 'enterprise' && (
              <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 12, marginBottom: 0 }}>
                <InfoCircleOutlined /> {missing.length} of {names.length} capabilities are not in this
                edition. They are gated in the gateway, not hidden in the browser — removing the badge
                does not grant them.
              </Paragraph>
            )}
          </Card>
        </Col>
      </Row>

      <Card
        size="small"
        title="Recent requests"
        extra={
          <Text type="secondary" style={{ fontSize: 12 }}>
            time to first token vs decode; a wide gap on short answers means KV-cache misses
          </Text>
        }
      >
        <Table<RecentSample>
          size="small"
          rowKey={(_, i) => String(i)}
          pagination={false}
          scroll={{ x: 720 }}
          dataSource={data.recent.slice(0, 12)}
          columns={[
            { title: 'Model', dataIndex: 'model', ellipsis: true },
            { title: 'Endpoint', dataIndex: 'endpoint', ellipsis: true, width: 140 },
            {
              title: 'Mode',
              dataIndex: 'streamed',
              width: 90,
              render: (s: boolean) => <Tag>{s ? 'stream' : 'blocking'}</Tag>,
            },
            { title: 'TTFT', dataIndex: 'ttftMs', width: 90, render: (v: number) => `${v} ms` },
            {
              title: 'Decode',
              dataIndex: 'durationMs',
              width: 100,
              render: (v: number, r) => `${Math.max(0, v - r.ttftMs)} ms`,
            },
            {
              title: 'Tokens',
              width: 130,
              render: (_, r) => `${r.promptTokens} → ${r.completionTokens}`,
            },
            {
              title: 'Settlement',
              dataIndex: 'usageKnown',
              width: 130,
              render: (known: boolean) =>
                known ? <Tag color="green">billed</Tag> : <Tag color="warning">estimated</Tag>,
            },
          ]}
        />
      </Card>

      {data.edition === 'enterprise' && (
        <Card size="small">
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label="Customer">{data.customer || '—'}</Descriptions.Item>
            <Descriptions.Item label="Licence expires">
              {data.expiresAt ? new Date(data.expiresAt).toLocaleString() : 'perpetual'}
            </Descriptions.Item>
            <Descriptions.Item label="Version">{data.version}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
    </Space>
  );
}

function EndpointRow({ ep }: { ep: EndpointStatus }) {
  const kv = ep.kvCacheUsed;
  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Text strong ellipsis style={{ maxWidth: 320 }}>
          {ep.model}
        </Text>
        <Badge status={ep.healthy ? 'success' : 'error'} text={ep.healthy ? 'healthy' : 'down'} />
      </Space>
      <Text type="secondary" style={{ fontSize: 12 }} ellipsis>
        {ep.baseUrl}
      </Text>
      <Space size="middle" style={{ marginTop: 8, width: '100%' }} align="center">
        <Tag>{ep.replicas}×</Tag>
        <Text type="secondary" style={{ fontSize: 12 }}>
          queue {ep.queueDepth}
        </Text>
        <Text type="secondary" style={{ fontSize: 12 }}>
          running {ep.runningRequests}
        </Text>
        {kv > 0 && (
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flex: 1 }}>
            <Progress percent={Math.round(kv * 100)} size="small" style={{ flex: 1, margin: 0 }} />
            <Text type="secondary" style={{ fontSize: 11 }}>
              KV {Math.round(kv * 100)}%
            </Text>
          </span>
        )}
      </Space>
    </div>
  );
}
