package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/render"
)

// observe decides which phase a deployment is actually in.
//
// This is the function the whole design exists for. From a Deployment's status
// alone, "waiting for a node" and "no node will ever match" are the same
// string. One is normal and the other needs the spec changed, and an operator
// who cannot tell them apart waits forever for a scheduling that cannot happen.
func (r *FleetDeploymentReconciler) observe(ctx context.Context, dep *api.FleetDeployment, res *render.Result) (api.DeploymentPhase, string, int32, int32) {
	got, err := r.workload(ctx, dep, res)
	if err != nil {
		return api.DeployPending, "workload not readable yet", 0, 0
	}

	desired := dep.Spec.Replicas
	if desired < 0 {
		desired = 0
	}
	ready := got.Status.ReadyReplicas
	report := func(phase api.DeploymentPhase, reason string) (api.DeploymentPhase, string, int32, int32) {
		return phase, reason, ready, got.Status.Replicas
	}

	switch {
	case desired > 0 && ready >= desired:
		return report(api.DeployAvailable, "")
	case ready > 0:
		return report(api.DeployScheduling, fmt.Sprintf("%d of %d replicas ready", ready, desired))
	}

	// Nothing is ready. Before reporting a wait, check whether the wait can
	// ever end: a deployment needing eight accelerators on a cluster with two
	// is not slow, it is impossible, and saying "scheduling" there is a lie
	// that costs the operator a day of watching a progress bar.
	if reason := r.capacityShortfall(ctx, dep, res); reason != "" {
		return report(api.DeployInsufficientCapacity, reason)
	}

	pods, err := r.podsFor(ctx, dep)
	if err != nil || len(pods) == 0 {
		return report(api.DeployScheduling, "the scheduler has not placed any pod yet")
	}
	return report(api.DeployScheduling, describe(pods))
}

// capacityShortfall is why no pod could ever be placed, or "" if the cluster
// has enough devices and the shortfall is somewhere else.
//
// Only device count is decided here, because it is the one shortfall this
// function can state with certainty from what it can see. A node that exists
// but lacks the right architecture, or a volume that will not attach, belongs
// to the scheduler, which reports it better than a count ever could.
func (r *FleetDeploymentReconciler) capacityShortfall(ctx context.Context, dep *api.FleetDeployment, res *render.Result) string {
	need := res.GPUPerReplica
	if need <= 0 {
		// An engine that asks for no devices cannot run short of them.
		return ""
	}
	have := r.clusterGPUs(ctx)
	if have < int(need) {
		return fmt.Sprintf("needs %d %s per replica; the cluster has %d",
			need, acceleratorName(dep), have)
	}
	if dep.Spec.Replicas > 0 && have*int(need) < int(dep.Spec.RequiredGPUs()) {
		return fmt.Sprintf("needs %d %s in total for %d replicas; the cluster has %d",
			dep.Spec.RequiredGPUs(), acceleratorName(dep), dep.Spec.Replicas, have)
	}
	return ""
}

// nvidiaGPU is the whole-device extended resource. It is a constant rather
// than a corev1 constant because Kubernetes has no built-in name for it: the
// device plugin defines the name, and the widely registered one is this.
const nvidiaGPU corev1.ResourceName = "nvidia.com/gpu"

// clusterGPUs counts allocatable whole NVIDIA devices across the cluster.
//
// Read through the uncached reader rather than the cache. Two reasons, and
// both are about the cache being the wrong tool: a namespace-scoped cache
// cannot list cluster-scoped Nodes at all, and a cache that has not yet
// synced reports an empty cluster, which reads as InsufficientCapacity when
// the truth is "ask again in a second".
func (r *FleetDeploymentReconciler) clusterGPUs(ctx context.Context) int {
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return 0
	}
	total := 0
	for _, n := range nodes.Items {
		q, ok := n.Status.Allocatable[nvidiaGPU]
		if !ok {
			continue
		}
		total += int(q.Value())
	}
	return total
}

func acceleratorName(dep *api.FleetDeployment) string {
	if dep.Spec.Resources != nil && dep.Spec.Resources.AcceleratorType != "" {
		return dep.Spec.Resources.AcceleratorType
	}
	return "nvidia.com/gpu"
}

// podsFor lists the pods a deployment owns, selected by the label the renderer
// stamped on them rather than by a field selector that would depend on the
// controller having set ownerReferences early enough.
func (r *FleetDeploymentReconciler) podsFor(ctx context.Context, dep *api.FleetDeployment) ([]corev1.Pod, error) {
	var pods corev1.PodList
	err := r.List(ctx, &pods, client.InNamespace(dep.Namespace),
		client.MatchingLabels{"fleet.zlogic.com/deployment": dep.Name})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// describe turns pending pod conditions into one readable sentence.
//
// Sorted, because a status reason that changes wording between two reconciles
// looks like a real change to anything watching the resource, and an operator
// reading two successive kubectl outputs should not have to diff them to see
// nothing happened.
func describe(pods []corev1.Pod) string {
	counts := map[string]int{}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			return "a pod is running but not yet ready"
		}
		for _, c := range p.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case corev1.PodScheduled:
				if c.Reason == corev1.PodReasonUnschedulable && c.Message != "" {
					counts[trim(c.Message)]++
				}
			case corev1.PodInitialized:
				if c.Reason == "ContainersNotInitialized" {
					counts["waiting on an init container"]++
				}
			}
		}
	}
	if len(counts) == 0 {
		return "pods exist but report no reason"
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if n := counts[r]; n > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", r, n))
		} else {
			out = append(out, r)
		}
	}
	return strings.Join(out, "; ")
}

// trim shortens a scheduler message to its first clause, which is the part
// naming a resource rather than listing every node that was considered.
func trim(msg string) string {
	if i := strings.Index(msg, "; "); i > 0 {
		return msg[:i]
	}
	if len(msg) > 140 {
		return msg[:140] + "..."
	}
	return msg
}
