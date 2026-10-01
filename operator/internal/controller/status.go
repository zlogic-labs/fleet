package controller

import (
	"context"
	"net"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/render"
)

// statusFor builds the status a reconcile concluded with.
//
// Everything a failure can also report is filled in here rather than at each
// call site, so that a deployment which failed admission still carries the
// replica count and the device count the operator asked for. Those two numbers
// are how an operator sizes a cluster, and a spec that was refused should still
// tell them what it would have needed.
func statusFor(dep *api.FleetDeployment, phase api.DeploymentPhase, reason string, res *render.Result) api.FleetDeploymentStatus {
	out := api.FleetDeploymentStatus{
		Phase:              phase,
		Reason:             reason,
		Engine:             dep.Spec.Engine,
		GPUPerReplica:      dep.Spec.GPUPerReplica(),
		RequiredGPUs:       dep.Spec.RequiredGPUs(),
		ObservedGeneration: dep.Generation,
	}
	if res != nil {
		out.Selector = res.Selector
		// The port belongs in the status, not just in the gateway's
		// configuration. An address without one is read as port 80 by
		// anything that dials it, and the engines listen on 8000 — so a
		// consumer that trusted this field would time out against a healthy
		// engine and conclude it was down.
		out.Address = res.Address
		if res.Port > 0 {
			out.Address = net.JoinHostPort(res.Address, strconv.Itoa(int(res.Port)))
		}
		if res.GPUPerReplica > 0 {
			out.GPUPerReplica = res.GPUPerReplica
		}
	}
	return out
}

// status writes the status back, tolerating a conflict.
//
// A conflict means someone wrote between our read and our write, and the
// reconcile is already scheduled again. Failing it outright would turn ordinary
// contention into an error in the controller's log and a pointless requeue.
func (r *FleetDeploymentReconciler) status(ctx context.Context, dep *api.FleetDeployment, next api.FleetDeploymentStatus) (ctrl.Result, error) {
	next = mergeStatus(dep.Status, next)
	dep.Status = next
	if err := r.Status().Update(ctx, dep); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// mergeStatus keeps what this reconcile did not compute.
//
// Replica counts come from the cluster rather than from a render, so a status
// write that zeroed them would make a healthy deployment that had just been
// reconciled look like one that had just appeared, and the gateway would drop
// its endpoint every time a spec changed.
func mergeStatus(cur, next api.FleetDeploymentStatus) api.FleetDeploymentStatus {
	out := next
	// Replica counts come from the cluster and are set by the caller; a zero
	// here means the workload was unreadable this pass, which is not the same
	// as a cluster reporting zero, so the previous value is kept rather than
	// overwriting it with a number nobody observed.
	if out.Replicas == 0 && out.ReadyReplicas == 0 {
		out.ReadyReplicas = cur.ReadyReplicas
		out.Replicas = cur.Replicas
	}
	if out.Selector == "" {
		out.Selector = cur.Selector
	}
	if out.Address == "" {
		out.Address = cur.Address
	}
	if out.Format == "" {
		out.Format = cur.Format
	}
	if out.Version == "" {
		out.Version = cur.Version
	}
	return out
}
