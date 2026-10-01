package report

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/zlogic-labs/fleet/core/pkg/inventory"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
)

// Collector turns cluster state into an inventory report.
//
// The uncached reader is mandatory here rather than optional. A cache scoped to
// one namespace cannot list cluster-scoped Nodes at all, and one that has not
// finished its initial sync reports an empty cluster — which the control plane
// would store as "this cluster has no nodes", a confident false statement that
// costs an operator their whole cluster view until something else refreshes it.
type Collector struct {
	Reader  client.Reader
	Scheme  *runtime.Scheme
	Version string
}

// Collect reads the cluster and builds a report.
func (c *Collector) Collect(ctx context.Context) (inventory.Report, error) {
	nodes, err := c.nodes(ctx)
	if err != nil {
		return inventory.Report{}, err
	}
	deps, err := c.deployments(ctx)
	if err != nil {
		return inventory.Report{}, err
	}
	return inventory.Report{
		Cluster: inventory.Cluster{
			Reachable: true,
			Version:   c.Version,
			Nodes:     nodes,
			// Totals are summed here rather than in the console, so the
			// summary cannot disagree with the node list underneath it. Only
			// ready nodes count: a node that is not Ready is not capacity
			// anyone can deploy onto, and pricing idle hardware as usable
			// invites a plan the cluster cannot execute.
			CPUMillis: totalCPU(nodes),
			MemoryMiB: totalMemory(nodes),
		},
		Deployments: deps,
	}, nil
}

// totalCPU and totalMemory sum the ready nodes' allocatable capacity.
//
// Allocatable rather than capacity: a node's capacity is the hardware, and its
// allocatable is what the scheduler will actually admit, which is already
// discounted by the system daemons consuming a slice of every GPU node.
func totalCPU(nodes []inventory.Node) int64 {
	var total int64
	for _, n := range nodes {
		if n.Ready {
			total += n.CPUMillis
		}
	}
	return total
}

func totalMemory(nodes []inventory.Node) int64 {
	var total int64
	for _, n := range nodes {
		if n.Ready {
			total += n.AllocatableMemoryMiB
		}
	}
	return total
}

func (c *Collector) nodes(ctx context.Context) ([]inventory.Node, error) {
	var list corev1.NodeList
	if err := c.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]inventory.Node, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, node(&list.Items[i]))
	}
	return out, nil
}

func (c *Collector) deployments(ctx context.Context) ([]inventory.Deployment, error) {
	var list api.FleetDeploymentList
	if err := c.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]inventory.Deployment, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		out = append(out, inventory.Deployment{
			Name:      d.Name,
			Namespace: d.Namespace,
			Model:     d.Spec.ModelRef,
			Desired:   int(d.Spec.Replicas),
			Ready:     int(d.Status.ReadyReplicas),
			TP:        int(d.Spec.TensorParallelSize),
			PP:        int(d.Spec.PipelineParallelSize),
			GPUPer:    int(d.Spec.GPUPerReplica()),
			Engine:    d.Spec.Engine,
			State:     inventory.DeployState(d.Status.Phase),
			Reason:    d.Status.Reason,
			Selector:  d.Status.Selector,
			Address:   d.Status.Address,
			Version:   d.Status.Version,
			Format:    d.Status.Format,
		})
	}
	return out, nil
}

// Run reports on an interval until ctx is cancelled.
//
// A failure is logged and the loop continues. The control plane being
// unreachable is not a reason for the operator to stop reconciling, and an
// operator that crashed because a sidecar was down would turn a reporting
// problem into an outage.
func (c *Collector) Run(ctx context.Context, rep *Reporter, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logger := log.FromContext(ctx).WithName("inventory")

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if rpt, err := c.Collect(ctx); err != nil {
			logger.Error(err, "could not read cluster state")
		} else if err := rep.Send(ctx, rpt); err != nil {
			logger.Error(err, "could not report inventory")
		} else {
			logger.V(1).Info("reported inventory",
				"nodes", len(rpt.Cluster.Nodes), "deployments", len(rpt.Deployments))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Start satisfies manager.Runnable.
func (c *Collector) Start(ctx context.Context) error { return nil }

// NeedLeaderElection reports whether this loop must only run on the leader.
//
// Yes: two operator replicas reporting the same cluster produce duplicate
// reports and, worse, let a standby overwrite the leader's fresher state with
// its own stale read at the wrong moment.
func (c *Collector) NeedLeaderElection() bool { return true }
