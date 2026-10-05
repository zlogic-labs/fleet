import {
  Alert,
  Badge,
  Button,
  Card,
  Descriptions,
  Empty,
  Progress,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { CloudServerOutlined, InfoCircleOutlined, ReloadOutlined } from '@ant-design/icons';

import { cluster as clusterApi, deployments as deploymentsApi } from '../api/control';
import { usePoll, humanAge } from '../hooks';
import { ControlPlaneAlert } from '../parts/control-plane';
import type { ClusterNode, ClusterReport, Deployment } from '../types';

const { Text, Paragraph } = Typography;

const DEPLOY_COLOR: Record<Deployment['state'], string> = {
  Available: 'green',
  Progressing: 'blue',
  Pending: 'default',
  Scheduling: 'gold',
  Degraded: 'orange',
  Failed: 'red',
};

export function Cluster() {
  const status = usePoll((signal) => clusterApi.get(signal), 5000);
  const deps = usePoll((signal) => deploymentsApi.list(signal), 5000);

  const reports = status.data?.clusters ?? [];
  const noControlPlane = status.error !== null;

  return (
    <Space direction="vertical" size="middle" style={{ width: '100%' }}>
      {noControlPlane && (
        <ControlPlaneAlert
          error={status.error}
          hint="Cluster inventory is reported by the operator. Start fleet-apiserver on port 8081."
        />
      )}

      {status.data && reports.length === 0 && (
        <Alert
          type="warning"
          showIcon
          message="Nothing has reported inventory yet"
          description={
            <>
              <Paragraph style={{ marginBottom: 8 }}>
                Fleet does not read Kubernetes itself. The gateway and the control plane are both
                forbidden from depending on it, so node and GPU inventory arrives from the operator
                running inside the cluster.
              </Paragraph>
              <Text type="secondary" style={{ fontSize: 12 }}>
                <InfoCircleOutlined /> Until the operator is deployed this page stays empty by
                design — see docs/architecture.md §3.
              </Text>
            </>
          }
        />
      )}

      {reports.map((report) => (
        <Report key={report.name} report={report} />
      ))}

      <Card
        size="small"
        title="Deployments"
        extra={
          <Space>
            <Text type="secondary" style={{ fontSize: 12 }}>
              FleetDeployment objects, reconciled by the operator
            </Text>
            <Button size="small" icon={<ReloadOutlined />} onClick={deps.refresh} />
          </Space>
        }
      >
        {!deps.data || deps.data.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="no deployments" />
        ) : (
          <Table<Deployment>
            size="small"
            rowKey={(r) => `${r.namespace}/${r.name}`}
            pagination={false}
            scroll={{ x: 800 }}
            dataSource={deps.data}
            columns={[
              {
                title: 'Name',
                dataIndex: 'name',
                ellipsis: true,
                render: (v: string, r) => (
                  <Space direction="vertical" size={0}>
                    <Text strong>{v}</Text>
                    <Text type="secondary" style={{ fontSize: 11 }}>
                      {r.namespace}
                    </Text>
                  </Space>
                ),
              },
              { title: 'Model', dataIndex: 'model', ellipsis: true },
              {
                title: 'Replicas',
                width: 120,
                render: (_, r) => (
                  <Space size={4}>
                    <Text style={{ fontSize: 12 }}>
                      {r.readyReplicas}/{r.desiredReplicas}
                    </Text>
                    {r.desiredReplicas !== r.readyReplicas && (
                      <Badge status={r.state === 'Failed' ? 'error' : 'warning'} />
                    )}
                  </Space>
                ),
              },
              {
                title: 'Parallelism',
                width: 130,
                render: (_, r) => (
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    TP {r.tensorParallelSize} · PP {r.pipelineParallelSize} · {r.gpuPerReplica} GPU
                  </Text>
                ),
              },
              {
                title: 'State',
                dataIndex: 'state',
                width: 120,
                render: (v: Deployment['state'], r) => (
                  <Tooltip title={r.reason}>
                    <Tag color={DEPLOY_COLOR[v]}>{v}</Tag>
                  </Tooltip>
                ),
              },
              {
                title: 'Age',
                width: 80,
                // The control plane sends a timestamp, not a duration: a
                // server-rendered "5m" goes stale between reports, and a
                // client-rendered one is wrong whenever the two clocks differ.
                render: (_, r) => (
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {humanAge(r.updatedAt, Date.now())}
                  </Text>
                ),
              },
            ]}
          />
        )}
      </Card>
    </Space>
  );
}

function Report({ report }: { report: ClusterReport }) {
  // No cluster summary here. The figures these cards held — nodes, GPUs, CPU,
  // memory — are on the Overview, where the question "is my pool being used"
  // is actually asked. Repeating them above a node table gave the console two
  // answers to one question and made a page nobody needed between the landing
  // page and the numbers.
  //
  // Cluster memoryMiB is the whole cluster's total while the per-node
  // Allocatable column below is the schedulable part, so the two could not be
  // summed or compared without a note. That note lived in a card comment.

  return (
    <Card
      size="small"
      title={
        <Space>
          <CloudServerOutlined />
          {report.name}
          <Tag color={report.reachable ? 'green' : 'red'}>{report.reachable ? 'reachable' : 'unreachable'}</Tag>
        </Space>
      }
      extra={
        <Text type="secondary" style={{ fontSize: 12 }}>
          reported {humanAge(report.reportedAt, Date.now())} ago
        </Text>
      }
    >
      {report.message && <Alert type="warning" message={report.message} style={{ marginBottom: 16 }} />}

      {report.nodes.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="no nodes reported" />
      ) : (
        <Table<ClusterNode>
          size="small"
          rowKey="name"
          pagination={false}
          scroll={{ x: 760 }}
          dataSource={report.nodes}
          columns={[
            {
              title: 'Node',
              dataIndex: 'name',
              ellipsis: true,
              render: (v: string, r) => (
                <Space>
                  <Badge status={r.ready ? 'success' : 'error'} />
                  <Text strong>{v}</Text>
                </Space>
              ),
            },
            {
              title: 'Roles',
              dataIndex: 'roles',
              width: 180,
              render: (roles: string[]) => (
                <Space size={4} wrap>
                  {roles.map((role) => (
                    <Tag key={role} style={{ fontSize: 11 }}>
                      {role}
                    </Tag>
                  ))}
                </Space>
              ),
            },
            {
              title: 'GPU',
              dataIndex: 'gpu',
              width: 200,
              render: (gpu: ClusterNode['gpu']) =>
                gpu.count ? (
                  <Space direction="vertical" size={0}>
                    <Text style={{ fontSize: 12 }}>
                      {gpu.count}× {gpu.model}
                    </Text>
                    <Text type="secondary" style={{ fontSize: 11 }}>
                      {Math.round(gpu.totalMemoryMiB / 1024)} GiB each
                    </Text>
                  </Space>
                ) : (
                  <Tag>none</Tag>
                ),
            },
            {
              title: 'Allocatable',
              dataIndex: 'allocatableMemoryMiB',
              width: 150,
              render: (v: number) => (
                <Space size={6} style={{ width: '100%' }}>
                  <Progress
                    percent={report.memoryMiB ? Math.min(100, Math.round((v / report.memoryMiB) * 100)) : 0}
                    size="small"
                    showInfo={false}
                    style={{ flex: 1, margin: 0 }}
                  />
                  <Text type="secondary" style={{ fontSize: 11 }}>
                    {Math.round(v / 1024)} GiB
                  </Text>
                </Space>
              ),
            },
            {
              title: 'Kubelet',
              dataIndex: 'kubeletVersion',
              width: 120,
              render: (v: string) => <Text type="secondary" style={{ fontSize: 12 }}>{v}</Text>,
            },
          ]}
          expandable={{
            expandedRowRender: (n) => (
              <Descriptions size="small" column={1} style={{ padding: '8px 0' }}>
                <Descriptions.Item label="OS">{n.osImage}</Descriptions.Item>
                <Descriptions.Item label="Addresses">
                  {Object.entries(n.addresses).map(([k, v]) => (
                    <Tag key={k} style={{ fontSize: 11 }}>
                      {k}: {v}
                    </Tag>
                  ))}
                </Descriptions.Item>
              </Descriptions>
            ),
          }}
        />
      )}
    </Card>
  );
}
