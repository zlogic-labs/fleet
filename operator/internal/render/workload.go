package render

import (
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
)

// defaultWeightsMount is where a rendered pod expects the weights to appear.
//
// A constant rather than a per-renderer path, because every engine takes a
// path and none of them should each invent one. A custom image that mounts
// somewhere else says so in spec.weightsMount.
const defaultWeightsMount = "/models"

// Weights returns the volume setup for a deployment's weights and the path the
// engine will find them at.
//
// The distinction it encodes is the one that decides cold-start time. A shared
// filesystem is mounted and read: nothing to copy, but every token generation
// may touch the network. Node-local is a copy to disk: minutes of staging, then
// local reads. Neither is right in general, which is why weightDelivery is a
// field of the deployment rather than a cluster-wide setting.
func Weights(in Input) (vol corev1.Volume, mountPath string, err error) {
	spec := in.Deployment.Spec
	mount := spec.WeightsMount
	if mount == "" {
		mount = defaultWeightsMount
	}
	storage := spec.Storage
	if storage == nil {
		return corev1.Volume{}, "", errs.InvalidArgument("spec.storage is required: the operator cannot guess which store the weights came from")
	}
	switch {
	case storage.HostPath != "":
		vol = corev1.Volume{
			Name: "weights",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: storage.HostPath},
			},
		}
	case storage.PVCName != "":
		vol = corev1.Volume{
			Name: "weights",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: storage.PVCName},
			},
		}
	default:
		return corev1.Volume{}, "", errs.InvalidArgument("spec.storage needs hostPath or pvcName")
	}
	return vol, mount, nil
}

// Engine renders a FleetDeployment into the Service and Deployment that serve
// it, given a renderer for the chosen engine.
//
// The two are rendered together because they share a selector and a name, and
// a Service whose selector matches nothing is a silent outage that looks
// exactly like a slow model.
func Engine(in Input, rd Renderer) (Result, error) {
	vol, mountPath, err := Weights(in)
	if err != nil {
		return Result{}, err
	}

	spec := in.Deployment.Spec
	modelName := selectorFor(in.Model)
	gpus := spec.GPUPerReplica()

	cmd, args := rd.Command(in)
	container := corev1.Container{
		Name:            "engine",
		Image:           in.Deployment.Spec.Image,
		Command:         cmd,
		Args:            args,
		Env:             engineEnv(in, mountPath),
		VolumeMounts:    []corev1.VolumeMount{{Name: "weights", MountPath: mountPath}},
		Resources:       resources(in, rd),
		ReadinessProbe:  healthProbe(rd, in),
		LivenessProbe:   livenessProbe(rd, in),
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
	if container.Image == "" {
		container.Image = rd.Image()
	}

	labels := objectMeta(in, "engine").Labels
	svcMeta := objectMeta(in, "service")
	svcMeta.Name = dnsName(in.Deployment.Name)
	svc := &corev1.Service{
		ObjectMeta: svcMeta,
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8000,
				TargetPort: intstr.FromInt32(8000),
			}},
			// The engine is not an ingress: no annotation, no host. The
			// gateway reaches it, and the gateway is the only thing in the
			// cluster with a reason to.
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	rt := in.Deployment.Spec.Replicas
	if rt < 0 {
		rt = 0
	}
	depMeta := objectMeta(in, "workload")
	depMeta.Name = svcMeta.Name
	dep := &appsv1.Deployment{
		ObjectMeta: depMeta,
		Spec: appsv1.DeploymentSpec{
			Replicas: &rt,
			// A large model takes minutes to load and a node failure should
			// not take the service down while the replacement warms up.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptrIntOrString(0),
					MaxSurge:       ptrIntOrString(1),
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"fleet.zlogic.com/weight-delivery": spec.WeightDelivery,
						"fleet.zlogic.com/format":          in.Model.Status.Format.String(),
					},
				},
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{container},
					Volumes:       []corev1.Volume{vol},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}

	if sel := nodeSelector(in, rd, gpus); len(sel) > 0 {
		dep.Spec.Template.Spec.NodeSelector = sel
	}

	return Result{
		Objects: []Object{
			{Kind: "Service", Name: svc.Name, Apply: applyService(svc)},
			{Kind: "Deployment", Name: dep.Name, Apply: applyDeployment(dep)},
		},
		Selector:      modelName,
		Address:       addressFor(svcMeta.Name, in.Deployment.Namespace),
		Name:          svcMeta.Name,
		GPUPerReplica: acceleratorsRequested(rd, in),
		Format:        in.Model.Status.Format,
	}, nil
}

// applyService copies the rendered Service onto whatever the cluster holds.
//
// Spec and labels are replaced wholesale rather than merged. A Service with
// one stale selector key and one new one selects nothing, and a partially
// applied Service is exactly how a deployment ends up serving with no endpoints
// and no error.
func applyService(desired *corev1.Service) func(client.Object) error {
	return func(dst client.Object) error {
		cur, ok := dst.(*corev1.Service)
		if !ok {
			return nil
		}
		cur.Labels = desired.Labels
		cur.Spec.Selector = desired.Spec.Selector
		cur.Spec.Ports = desired.Spec.Ports
		cur.Spec.Type = desired.Spec.Type
		return nil
	}
}

func applyDeployment(desired *appsv1.Deployment) func(client.Object) error {
	return func(dst client.Object) error {
		cur, ok := dst.(*appsv1.Deployment)
		if !ok {
			return nil
		}
		cur.Labels = desired.Labels
		cur.Spec.Replicas = desired.Spec.Replicas
		cur.Spec.Selector = desired.Spec.Selector
		cur.Spec.Strategy = desired.Spec.Strategy
		cur.Spec.Template = desired.Spec.Template
		return nil
	}
}

// engineEnv is the environment every rendered pod gets.
//
// HF_HOME is set and HF_HUB_OFFLINE is forced because an engine that reaches
// for a tokenizer over the network will hang in a cluster with no egress, and
// the resulting timeout is reported as an engine crash. spec.env is applied
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
				Port: intstr.FromInt32(8000),
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
