package report

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// node is one Node as the console shows it.
//
// The GPU figures come from the device plugin's extended resource rather than
// from NVML, because that is the number the scheduler actually admits against.
// Reporting what the hardware is while the scheduler plans on something else
// is how an operator ends up with a cluster that looks big enough and still
// cannot place a replica.
func node(n *corev1.Node) inventory.Node {
	out := inventory.Node{
		Name:       n.Name,
		Ready:      nodeReady(n),
		Roles:      roles(n),
		Kubelet:    n.Status.NodeInfo.KubeletVersion,
		OSImage:    n.Status.NodeInfo.OSImage,
		Addresses:  map[string]string{},
		ReportedAt: n.CreationTimestamp.Time,
	}
	if q, ok := n.Status.Allocatable[nvidiaGPU]; ok {
		out.GPU.Count = int(q.Value())
	}
	out.CPUMillis = n.Status.Allocatable.Cpu().MilliValue()
	out.AllocatableMemoryMiB = n.Status.Allocatable.Memory().Value() / (1 << 20)
	out.GPU.TotalMemMiB = gpuMemoryMiB(n)

	for _, addr := range n.Status.Addresses {
		// First writer wins, except that InternalIP replaces whatever a less
		// useful type already claimed the slot.
		key := string(addr.Type)
		if addr.Type == corev1.NodeInternalIP || out.Addresses[key] == "" {
			out.Addresses[key] = addr.Address
		}
	}
	return out
}

// nvidiaGPU is the whole-device extended resource. It is a local constant
// rather than an upstream one because Kubernetes has no built-in name for it:
// the device plugin defines it, and this is the one almost every node registers.
const nvidiaGPU corev1.ResourceName = "nvidia.com/gpu"

// gpuMemoryMiB sums advertised GPU memory across the device plugin's
// per-device resources.
//
// The plugin advertises each card's memory as a separate extended resource
// whose name carries the model's slug, so the total is found by pattern rather
// than by a fixed name. A cluster whose plugin publishes only the device count
// leaves this at zero, which the console shows as unknown rather than as none
// — the difference matters when someone is sizing a cluster.
func gpuMemoryMiB(n *corev1.Node) int64 {
	var total int64
	for name, q := range n.Status.Capacity {
		s := string(name)
		if strings.HasPrefix(s, "nvidia.com/gpu.memory") {
			total += q.Value() / (1 << 20)
		}
	}
	return total
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// roles are the labels a node announces about what it does.
//
// An unlabelled node is reported as a worker. That is not a guess about intent:
// a node with no role label is schedulable for workloads, so calling it
// anything else would tell an operator their node is unusable when it is
// placing Pods. A control-plane-only node keeps just its control-plane role.
func roles(n *corev1.Node) []string {
	var out []string
	_, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
	if !isControlPlane {
		_, isControlPlane = n.Labels["node-role.kubernetes.io/master"]
	}
	if isControlPlane {
		out = append(out, "control-plane")
	}
	_, isWorker := n.Labels["node-role.kubernetes.io/worker"]
	if isWorker || !isControlPlane {
		out = append(out, "worker")
	}
	return out
}
