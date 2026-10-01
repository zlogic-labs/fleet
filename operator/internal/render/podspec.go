package render

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
)

// The pod template's innards.
//
// Separate from workload.go because workload.go is the shape of the objects a
// deployment becomes, and this is the shape of one container. They change for
// different reasons: adding a probe has nothing to do with how a Service is
// named.

// last, so an operator can override any of it.
func engineEnv(in Input, mountPath string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "FLEET_MODEL", Value: selectorFor(in.Model)},
		{Name: "FLEET_WEIGHTS", Value: mountPath},
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "HF_HOME", Value: mountPath + "/.hf"},
	}
	return append(env, specEnv(in)...)
}

// specEnv is an operator's explicit environment, sorted so a rendered pod is
// byte-identical across reconciles. A map iterated in random order makes every
// apply look like a change and floods the API server with no-op updates.
func specEnv(in Input) []corev1.EnvVar {
	keys := make([]string, 0, len(in.Deployment.Spec.Env))
	for k := range in.Deployment.Spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: in.Deployment.Spec.Env[k]})
	}
	return out
}

// resources turns a deployment's resource request into a container's.
//
// The accelerator request is separate because it is a device, not a resource:
// a node with one GPU is a node that can hold a TP=1 replica and nothing more,
// and expressing that as a limit would let the scheduler place two.
func resources(in Input, rd Renderer) corev1.ResourceRequirements {
	req := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	spec := in.Deployment.Spec
	if spec.Resources != nil {
		if spec.Resources.CPU != "" {
			req.Requests[corev1.ResourceCPU] = resource.MustParse(spec.Resources.CPU)
			req.Limits[corev1.ResourceCPU] = resource.MustParse(spec.Resources.CPU)
		}
		if spec.Resources.Memory != "" {
			req.Requests[corev1.ResourceMemory] = resource.MustParse(spec.Resources.Memory)
			req.Limits[corev1.ResourceMemory] = resource.MustParse(spec.Resources.Memory)
		}
	}
	name := accelResource(spec, rd)
	if name != "" {
		n := acceleratorsRequested(rd, in)
		if n > 0 {
			q := resource.NewQuantity(int64(n), resource.DecimalSI)
			req.Limits[corev1.ResourceName(name)] = *q
			req.Requests[corev1.ResourceName(name)] = *q
		}
	}
	if len(req.Requests) == 0 {
		req = corev1.ResourceRequirements{}
	}
	return req
}

// accelResource is the extended resource name to ask for, if any.
func accelResource(spec api.FleetDeploymentSpec, rd Renderer) string {
	if spec.Resources != nil && spec.Resources.AcceleratorType != "" {
		return spec.Resources.AcceleratorType
	}
	// The zero Input is deliberate: this asks what the renderer wants in
	// general, and only the deployment's own override can change the answer.
	name, _ := rd.Accelerators(Input{})
	return name
}

func acceleratorsRequested(rd Renderer, in Input) int32 {
	spec := in.Deployment.Spec
	if spec.Resources != nil && spec.Resources.AcceleratorCount > 0 {
		return spec.Resources.AcceleratorCount
	}
	_, n := rd.Accelerators(in)
	return n
}

// nodeSelector pins the pod to nodes that can hold it.
//
// Only applied when the renderer asked for a named device: a CPU-only engine
// must not inherit a GPU label from a cluster default, or it would sit
// unschedulable on a cluster that has no GPU at all.
func nodeSelector(in Input, rd Renderer, gpus int32) map[string]string {
	spec := in.Deployment.Spec
	name := accelResource(spec, rd)
	if name == "" || gpus <= 0 {
		return nil
	}
	switch name {
	case "nvidia.com/gpu":
		if spec.Resources != nil && spec.Resources.AcceleratorType == "" && spec.WeightDelivery == "nodeLocal" {
			return map[string]string{"nvidia.com/gpu.present": "true"}
		}
	}
	return nil
}

// healthProbe is the readiness gate.
//
// A slow start is set generously rather than tuned: a 671B replica taking
// twenty minutes to load is normal, and a probe that gives up at five makes
// the scheduler kill and restart the very process that was making progress.
func healthProbe(rd Renderer, in Input) *corev1.Probe {
	path := rd.HealthPath(in)
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(EnginePort),
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    120,
	}
}

func livenessProbe(rd Renderer, in Input) *corev1.Probe {
	p := healthProbe(rd, in)
	// Liveness must not fire while a model is still loading, or it will kill
	// the pod during exactly the window it needs to survive.
	p.FailureThreshold = 60
	p.PeriodSeconds = 30
	return p
}

// ptrIntOrString is the pointer a rolling-update threshold takes, which is a
// quantity rather than an int so that "25%" is expressible.
func ptrIntOrString(v int32) *intstr.IntOrString {
	n := intstr.FromInt32(v)
	return &n
}
