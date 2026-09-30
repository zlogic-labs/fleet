// Package v1alpha1 contains the Fleet custom resources.
//
// The operator's whole reason to exist is that a deployment is not a Pod: a
// Pod is one attempt at a deployment, and the interesting states are the ones
// where no Pod exists yet because the cluster cannot satisfy the request.
//
// The object:generate marker below is not optional, and its absence is not
// reported: without it controller-gen emits DeepCopyInto for the root types
// only, while still writing calls to the Spec and Status functions it never
// generated. The result is a package that does not compile, produced by a
// command that exits zero and prints nothing.
//
// The markers come last, immediately above the package clause, because
// controller-gen collects package markers from the doc comment and a stray
// paragraph after them yields a CRD with an empty group and version rather
// than an error.
//
// +kubebuilder:object:generate=true
// +groupName=fleet.zlogic.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version these types live in.
	GroupVersion = schema.GroupVersion{Group: "fleet.zlogic.com", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme is what a manager's scheme needs called on it.
	AddToScheme = SchemeBuilder.AddToScheme
)
