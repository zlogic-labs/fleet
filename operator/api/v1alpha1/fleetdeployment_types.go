package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetDeployment is a request to serve a model at a given size, and the state
// of that request.
//
// It is deliberately not a Pod template. A Pod is one scheduler attempt; the
// states that matter — not enough GPUs, no node with the right architecture,
// weights not staged yet — are all states where no Pod exists. Reading them off
// a Deployment loses exactly the information an operator needs.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.requiredGpus`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FleetDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetDeploymentSpec   `json:"spec,omitempty"`
	Status FleetDeploymentStatus `json:"status,omitempty"`
}

// FleetDeploymentSpec is the whole of an operator's intent.
type FleetDeploymentSpec struct {
	// ModelRef names a FleetModel. Cross-namespace references are refused: a
	// deployment that silently depended on another namespace's pull is a
	// deployment that breaks when the other namespace is cleaned up.
	ModelRef string `json:"modelRef"`
	// Engine is an engine family, looked up in the profile registry. It is
	// data, not a branch: there is no "if vllm" anywhere in this operator.
	Engine string `json:"engine"`
	// Image overrides the engine image from the profile. Empty means the
	// profile's default, which is what most deployments want.
	Image string `json:"image,omitempty"`
	// Replicas is the desired count. Pinned parallelism multiplies into it:
	// one replica of a TP=8 deployment is eight GPUs.
	Replicas int32 `json:"replicas,omitempty"`

	// TensorParallelSize and PipelineParallelSize are the intra-replica split.
	// They are what makes a single replica consume more than one GPU, and
	// they are why horizontal scaling is preferred: adding replicas costs
	// nothing but scheduling, while adding cards to a replica costs all-to-all
	// traffic on every token.
	TensorParallelSize   int32 `json:"tensorParallelSize,omitempty"`
	PipelineParallelSize int32 `json:"pipelineParallelSize,omitempty"`

	// EngineOptions are opaque to the operator. A key the engine does not
	// understand is an error the engine reports, not a validation failure in
	// here: the operator does not know every flag every engine will grow.
	EngineOptions map[string]string `json:"engineOptions,omitempty"`

	// Env is extra environment for the engine container, applied after the
	// operator's own. It is separate from EngineOptions because one is flags
	// and the other is environment, and a map that meant both would make
	// "CUDA_VISIBLE_DEVICES" ambiguous between them.
	Env map[string]string `json:"env,omitempty"`

	// Resources overrides the profile's default requests and limits.
	Resources *ResourceSpec `json:"resources,omitempty"`

	// ContextLength is the KV cache budget in tokens. It is the single
	// largest driver of KV cache memory, so it is worth stating rather than
	// inheriting whatever the engine defaults to.
	ContextLength int32 `json:"contextLength,omitempty"`

	// WeightDelivery is "shared" or "nodeLocal", defaulting to shared. A
	// safetensors checkpoint large enough to need a cluster is not copyable
	// per replica; a GGUF is small enough that copying it is faster than
	// reading it over a network filesystem on every cold start.
	WeightDelivery string `json:"weightDelivery,omitempty"`

	// WeightsMount is the directory the weights appear at inside the
	// container. The operator does not choose it; the renderer does, and this
	// is where a custom image that expects another path says so.
	WeightsMount string `json:"weightsMount,omitempty"`

	// Storage declares where the weights come from. Absent means the
	// operator's configured default, which is the only sane reading for a
	// single-cluster install.
	Storage *StorageSpec `json:"storage,omitempty"`
}

// ResourceSpec is a container's resource envelope.
type ResourceSpec struct {
	CPU              string `json:"cpu,omitempty"`
	Memory           string `json:"memory,omitempty"`
	AcceleratorCount int32  `json:"acceleratorCount,omitempty"`
	// AcceleratorType is an extended resource name such as
	// "nvidia.com/mig-1g.10gb". Empty asks for whole devices.
	AcceleratorType string `json:"acceleratorType,omitempty"`
}

// StorageSpec points at the object store the puller wrote to.
type StorageSpec struct {
	// Prefix is the store-relative directory, normally a FleetModel's
	// status.prefix.
	Prefix string `json:"prefix"`
	// HostPath mounts a directory from the node instead of a volume. It is a
	// single-cluster development affordance and is named as one.
	HostPath string `json:"hostPath,omitempty"`
	// PVCName mounts a claim instead, which is how a shared filesystem is
	// consumed.
	PVCName string `json:"pvcName,omitempty"`
}

// DeploymentPhase is where a FleetDeployment has got to.
//
// Scheduling and InsufficientCapacity are separate because they are different
// failures. "Waiting for a node" and "no node will ever match" look identical
// from outside a Deployment's status, and an operator cannot act on the first
// and must act on the second.
type DeploymentPhase string

const (
	// DeployPending means the request has not been acted on yet.
	DeployPending DeploymentPhase = "Pending"
	// DeployScheduling means a workload exists and is waiting for capacity.
	DeployScheduling DeploymentPhase = "Scheduling"
	// DeployInsufficientCapacity means the request cannot be scheduled as
	// specified: not enough accelerators, no node with the architecture, or
	// no room for the weight volume.
	DeployInsufficientCapacity DeploymentPhase = "InsufficientCapacity"
	// DeployAvailable means at least one replica is serving.
	DeployAvailable DeploymentPhase = "Available"
	// DeployFailed means the workload was rejected rather than merely
	// unscheduled: an incompatible engine, or weights that are not there.
	DeployFailed DeploymentPhase = "Failed"
)

// FleetDeploymentStatus is the operator's account of the world.
type FleetDeploymentStatus struct {
	Phase  DeploymentPhase `json:"phase,omitempty"`
	Reason string          `json:"reason,omitempty"`
	// GPUPerReplica is TensorParallelSize times PipelineParallelSize, the
	// number an operator needs to size a cluster against.
	GPUPerReplica int32 `json:"gpuPerReplica,omitempty"`
	// RequiredGPUs is GPUPerReplica times Replicas, the cluster-wide figure.
	RequiredGPUs int32 `json:"requiredGpus,omitempty"`

	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	Replicas      int32 `json:"replicas,omitempty"`

	// Address is the in-cluster endpoint the gateway routes to. It is a
	// Service DNS name, not a Pod IP, so it survives every rescheduling the
	// cluster does.
	Address string `json:"address,omitempty"`
	// Selector is the model name clients send, which is what makes
	// replicas of one deployment a single endpoint to the gateway.
	Selector string `json:"selector,omitempty"`

	// Engines is the probe result, copied from the engine profile's view of
	// the running pod. It is what proves P4: the version here is the one the
	// engine reported, not one Fleet assumed.
	Engine  string `json:"engine,omitempty"`
	Version string `json:"version,omitempty"`
	Format  string `json:"format,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// GPUPerReplica is the intra-replica device count, at least one.
func (s *FleetDeploymentSpec) GPUPerReplica() int32 {
	n := s.TensorParallelSize * s.PipelineParallelSize
	if n < 1 {
		return 1
	}
	return n
}

// RequiredGPUs is the cluster-wide device count for the desired size.
func (s *FleetDeploymentSpec) RequiredGPUs() int32 {
	r := s.Replicas
	if r < 0 {
		r = 0
	}
	return s.GPUPerReplica() * r
}

// +kubebuilder:object:root=true

// FleetDeploymentList is a list of FleetDeployment.
type FleetDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetDeployment{}, &FleetDeploymentList{})
}
