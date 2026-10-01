// Package controller reconciles Fleet resources against a cluster.
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/render"
)

// FleetDeploymentReconciler serves a FleetDeployment.
type FleetDeploymentReconciler struct {
	client.Client
	// Reader is the uncached API reader, used for cluster-scoped resources.
	// It must not be the cache: an operator scoped to one namespace has a
	// cache that cannot answer for Nodes, and cluster inventory read through
	// it fails with "unknown namespace for the cache" on every reconcile.
	Reader    client.Reader
	Scheme    *runtime.Scheme
	Renderers *render.Registry
	Profiles  *engine.Profiles
}

// Reconcile brings the cluster in line with one FleetDeployment.
//
// The order is the design: resolve the model, resolve the renderer, check the
// engine can load what the model turned out to be, then render. Everything
// decidable without a scheduler is decided before a Pod exists, because the
// states where no Pod exists are the ones an operator cannot see.
func (r *FleetDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("deployment", req.NamespacedName)

	var dep api.FleetDeployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !dep.DeletionTimestamp.IsZero() {
		// Garbage collection owns deletion: the rendered objects carry an
		// owner reference, so removing the CRD removes them. Deleting them
		// here as well would race it.
		return ctrl.Result{}, nil
	}

	model, err := r.resolveModel(ctx, &dep)
	if err != nil {
		return ctrl.Result{}, err
	}
	if model == nil {
		// Not an error: nothing will change until someone creates the model,
		// and the cache watch brings us back when they do.
		return r.status(ctx, &dep, statusFor(&dep, api.DeployPending,
			fmt.Sprintf("model %q is not available yet", dep.Spec.ModelRef), nil))
	}

	profile := r.profile(dep.Spec.Engine)
	if err := checkFormat(profile, model.Status.Format); err != nil {
		return r.status(ctx, &dep, statusFor(&dep, api.DeployFailed, err.Error(), nil))
	}

	rd, err := r.renderer(dep.Spec.Engine)
	if err != nil {
		return r.status(ctx, &dep, statusFor(&dep, api.DeployFailed, err.Error(), nil))
	}

	res, err := render.Engine(render.Input{Deployment: dep, Model: *model, Profile: profile}, rd)
	if err != nil {
		return r.status(ctx, &dep, statusFor(&dep, api.DeployFailed, err.Error(), nil))
	}
	if err := r.apply(ctx, res.Objects, &dep); err != nil {
		return ctrl.Result{}, err
	}

	phase, reason, ready, replicas := r.observe(ctx, &dep, &res)
	logger.V(1).Info("reconciled", "phase", phase, "reason", reason, "address", res.Address)
	st := statusFor(&dep, phase, reason, &res)
	st.ReadyReplicas, st.Replicas = ready, replicas
	st.Format = string(model.Status.Format)

	// Only ask a running engine what it is. A deployment that is still
	// scheduling has nothing to report, and a failed probe leaves the version
	// unset rather than carrying a guess into the console (P4).
	//
	// The Service ClusterIP is preferred over the DNS name for the probe, and
	// the distinction is not cosmetic: cluster.local only resolves from
	// inside the cluster, so an operator running on a workstation probes a
	// name it cannot resolve and reports no version for a perfectly healthy
	// engine. The IP works from both places. The DNS name still goes in the
	// status, because it is the address that survives rescheduling and it is
	// what the in-cluster gateway should use.
	if phase == api.DeployAvailable && res.Address != "" {
		target := r.probeTarget(ctx, &dep, &res)
		if facts := r.probeEndpoint(ctx, &dep, target, profile); facts.Version != "" {
			st.Version = facts.Version
		}
	}
	return r.status(ctx, &dep, st)
}

// resolveModel returns the model, or nil with a status explaining why not.
func (r *FleetDeploymentReconciler) resolveModel(ctx context.Context, dep *api.FleetDeployment) (*api.FleetModel, error) {
	if dep.Spec.ModelRef == "" {
		return nil, nil
	}
	var model api.FleetModel
	key := client.ObjectKey{Namespace: dep.Namespace, Name: dep.Spec.ModelRef}
	if err := r.Get(ctx, key, &model); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if model.Status.Phase != api.ModelReady {
		return nil, nil
	}
	return &model, nil
}

// checkFormat refuses a deployment the engine provably cannot serve.
//
// This is the whole reason Format is recorded on the model rather than guessed
// at render time: deploying a GGUF to vLLM is a certainty, and a certainty
// should cost one error message rather than a crash-looping Pod and a log.
func checkFormat(profile engine.Profile, have weights.Format) error {
	return engine.Compatible(have, profile.Name)
}

func (r *FleetDeploymentReconciler) profile(name string) engine.Profile {
	if r.Profiles != nil {
		return r.Profiles.For(name)
	}
	return engine.BuiltinProfiles().For(name)
}

func (r *FleetDeploymentReconciler) renderer(name string) (render.Renderer, error) {
	if r.Renderers != nil {
		return r.Renderers.For(name)
	}
	return render.Builtin().For(name)
}
