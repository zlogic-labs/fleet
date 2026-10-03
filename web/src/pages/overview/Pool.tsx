import { Progress, Typography } from 'antd';

import { Group, Measures } from './parts';
import type { ClusterReport, Deployment } from '../../types';

const { Text, Paragraph } = Typography;

// The pool: what the operator has, and how much of it is spoken for.
//
// "Committed" is the figure to read, and it is not utilisation. It is the share
// of the fleet a running model has claimed, which is the number that answers
// "would another GPU do anything" — a fleet at 100% committed cannot take more
// traffic, and a fleet at 10% committed is idle whatever any gauge says.

export function Pool({
  report,
  deployments,
  now,
}: {
  report: ClusterReport | undefined;
  deployments: Deployment[];
  now: number;
}) {
  if (!report) {
    return (
      <Group title="Pool">
        <Text type="secondary">
          No cluster has reported inventory. Node and GPU capacity arrives from the operator
          running inside the cluster.
        </Text>
      </Group>
    );
  }

  const committed = deployments.reduce(
    (a, d) => a + d.readyReplicas * d.gpuPerReplica,
    0,
  );
  const gpus = report.gpuCount;
  const percent = gpus > 0 ? Math.round((committed / gpus) * 100) : 0;

  return (
    <Group
      title="Pool"
      note={
        report.reportedAt
          ? `reported ${Math.max(0, Math.round((now - Date.parse(report.reportedAt)) / 1000))}s ago`
          : undefined
      }
      to={{ path: '/cluster', label: 'cluster' }}
    >
      <Measures
        items={[
          {
            // One figure, not two. "GPUs ready" and "GPUs committed" were both
            // rendered and on a healthy fleet they are the same pair of
            // numbers, so the page said it twice. They diverge exactly when a
            // deployment is pending or a node has dropped, and that divergence
            // is the thing worth seeing — so it is the hint that moves.
            label: 'GPUs',
            value: gpus > 0 ? `${report.readyGpus} of ${gpus} ready` : '—',
            hint:
              committed !== report.readyGpus
                ? `${committed} committed by a ready replica`
                : `${committed} committed`,
          },
          {
            label: 'CPU',
            value: `${(report.cpuMillicores / 1000).toFixed(report.cpuMillicores % 1000 ? 1 : 0)} cores`,
            hint: 'allocatable, not in use',
          },
          {
            label: 'Memory',
            value: `${(report.memoryMiB / 1024).toFixed(0)} GiB`,
            hint: 'allocatable, not in use',
          },
          {
            label: 'Nodes',
            value: `${report.readyNodes} of ${report.nodeCount} ready`,
          },
          {
            label: 'Deployments',
            value: String(deployments.length),
            hint: deployments.length ? deployments[0]?.namespace : undefined,
          },
        ]}
      />

      {gpus > 0 && (
        <div style={{ marginTop: 20, maxWidth: 400 }}>
          {/* Not coloured. A green bar at 100% would say "all good", and full
              commitment is not a verdict: it is either an efficiently used
              fleet or one with no headroom, and the number cannot tell those
              apart. The caption carries the meaning instead. */}
          <Progress percent={percent} size="small" showInfo={false} strokeColor="#1677ff" />
          <Text type="secondary" style={{ fontSize: 11 }}>
            {percent}% of the ready GPUs are claimed by a running model
          </Text>
        </div>
      )}

      <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 18, marginBottom: 0 }}>
        Fleet does not sample CPU or GPU utilisation. vLLM exports no utilisation metric, and an
        LLM decode loop pins the GPU at 100% while it is working, so a gauge here would read as a
        constant rather than as a measurement. The figures above are capacity and commitments —
        both real, and neither of them a load average.
      </Paragraph>
    </Group>
  );
}