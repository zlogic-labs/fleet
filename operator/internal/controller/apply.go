package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/render"
)

// apply writes the rendered objects, creating or updating as needed.
//
// CreateOrUpdate rather than server-side apply: SSA needs a field manager and a
// diff of every field Fleet has ever set, and a Deployment an operator edits by
// hand afterwards is then fought over forever. CreateOrUpdate lets the rendered
// object win outright, which is what "this is what a FleetDeployment means"
// should do.
func (r *FleetDeploymentReconciler) apply(ctx context.Context, objects []render.Object, owner *api.FleetDeployment) error {
	for _, obj := range objects {
		target, err := emptyFor(obj.Kind)
		if err != nil {
			return err
		}
		// The identity has to be on the object before CreateOrUpdate, because
		// it reads the existing one by the target's own name and namespace.
		// An unset namespace makes it fetch the path "/", which a
		// namespace-scoped cache refuses and reports as an unknown namespace.
		target.SetNamespace(owner.Namespace)
		target.SetName(obj.Name)

		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
			if obj.Apply != nil {
				if err := obj.Apply(target); err != nil {
					return err
				}
			}
			return controllerutil.SetControllerReference(owner, target, r.Scheme)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func emptyFor(kind string) (client.Object, error) {
	switch kind {
	case "Service":
		return &corev1.Service{}, nil
	case "Deployment":
		return &appsv1.Deployment{}, nil
	}
	return nil, errs.InvalidArgument("cannot render an object of kind %q", kind)
}

// workload reads back the Deployment a reconcile just wrote, to report what the
// cluster actually has rather than what was asked for.
//
// The name comes from the render result, not from the FleetDeployment: a
// Service cannot hold the dots a model name usually carries, so the rendered
// name is a derived one and using dep.Name here would look for an object that
// was never created — which reads as a transient failure and leaves the status
// stuck on the first reconcile, forever.
func (r *FleetDeploymentReconciler) workload(ctx context.Context, dep *api.FleetDeployment, res *render.Result) (*appsv1.Deployment, error) {
	var got appsv1.Deployment
	key := types.NamespacedName{Name: res.Name, Namespace: dep.Namespace}
	if err := r.Get(ctx, key, &got); err != nil {
		return nil, err
	}
	return &got, nil
}
