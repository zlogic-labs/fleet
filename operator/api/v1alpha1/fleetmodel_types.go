package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// FleetModel is a model's identity and provenance, independent of where or how
// many times it runs.
//
// It is a separate resource from FleetDeployment because a 671B checkpoint is
// 1.3 TB: two deployments of the same model must share one copy, and a
// reference is the only thing that makes sharing expressible.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Format",type=string,JSONPath=`.status.format`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FleetModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetModelSpec   `json:"spec,omitempty"`
	Status FleetModelStatus `json:"status,omitempty"`
}

// FleetModelSpec declares what to fetch and what it is.
//
// Format is inferred from the repository rather than declared, because the
// community publishes the same model in both layouts and the operator should
// not have to know which. It is still carried on the spec so an operator can
// state it and be told when reality disagrees.
type FleetModelSpec struct {
	// Source is a repository reference, "owner/name", with an optional
	// "@revision" suffix.
	Source string `json:"source"`
	// Format overrides inference. Empty means infer.
	Format weights.Format `json:"format,omitempty"`
	// TokenizerID names a tiktoken encoding. Required for safetensors,
	// meaningless for GGUF, which embeds its own.
	TokenizerID string `json:"tokenizerId,omitempty"`
	// ContextLimit is the model's maximum context in tokens. Zero means the
	// engine's default.
	ContextLimit int32 `json:"contextLimit,omitempty"`
}

// ModelPhase is where a FleetModel has got to.
type ModelPhase string

const (
	// ModelPending means the bytes are not in the store yet.
	ModelPending ModelPhase = "Pending"
	// ModelReady means the weights are stored and verified.
	ModelReady ModelPhase = "Ready"
	// ModelFailed means the pull or the verification failed.
	ModelFailed ModelPhase = "Failed"
)

// FleetModelStatus is what the operator observed, not what it hoped for.
type FleetModelStatus struct {
	Phase  ModelPhase `json:"phase,omitempty"`
	Reason string     `json:"reason,omitempty"`
	// Format is the format actually found in the repository, which may differ
	// from the one requested. Reporting the difference is the point.
	Format weights.Format `json:"format,omitempty"`
	// Prefix is the store-relative directory the weights occupy. A deployment
	// mounts this, so it is the contract between the two resources.
	Prefix             string      `json:"prefix,omitempty"`
	Objects            int         `json:"objects,omitempty"`
	SizeBytes          int64       `json:"sizeBytes,omitempty"`
	ContextLimit       int32       `json:"contextLimit,omitempty"`
	MissingFiles       []string    `json:"missingFiles,omitempty"`
	Files              []ModelFile `json:"files,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
}

// ModelFile is one object in the store, repository-relative.
//
// The sizes are not decoration. A multimodal GGUF repository also ships a
// vision projector as a second .gguf, and it is much the smaller of the two;
// a renderer that took the first match would point the engine at the projector
// and get a model that answers nothing.
type ModelFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes,omitempty"`
}

// +kubebuilder:object:root=true

// FleetModelList is a list of FleetModel.
type FleetModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetModel{}, &FleetModelList{})
}
