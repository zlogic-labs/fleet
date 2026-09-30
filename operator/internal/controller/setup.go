package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/render"
)

// New registers every controller with a manager.
//
// A single entry point rather than setup in main: a controller added here is a
// controller that exists, and one forgotten in main is an operator that
// silently reconciles half the resources it claims to own.
func New(mgr ctrl.Manager) error {
	r := &FleetDeploymentReconciler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Renderers: render.Builtin(),
		Profiles:  engine.BuiltinProfiles(),
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.FleetDeployment{}, builder.WithPredicates(changed())).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// A deployment whose model is not ready yet has nothing to watch, so
		// without this it sits Pending until something else happens to touch
		// it. The moment the pull finishes, its deployments are reconciled.
		Watches(&api.FleetModel{}, handler.EnqueueRequestsFromMapFunc(r.deploymentsForModel)).
		Named("fleetdeployment").
		Complete(r)
}

// deploymentsForModel maps a model to the deployments that wait on it.
func (r *FleetDeploymentReconciler) deploymentsForModel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list api.FleetDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ModelRef == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: list.Items[i].Namespace,
					Name:      list.Items[i].Name,
				},
			})
		}
	}
	return out
}

// changed ignores reconciles triggered by something other than a spec change
// or a deletion.
//
// A status subresource write is one of the things this controller itself does,
// so without this every reconcile requeues the next one and the loop runs at
// the API server's speed forever. Generation changes and deletions still pass.
func changed() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)
}
