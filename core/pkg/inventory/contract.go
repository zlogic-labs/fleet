package inventory

import "fmt"

// ContractVersion is the version of this wire contract that a build speaks. It
// travels in every report so the control plane can tell a newer operator from an
// older one, instead of inferring it from which fields happened to arrive.
const ContractVersion = 1

// Path is where a report is PUT. The version lives in the path, not in a
// header, so that an operator pinned to an old control plane fails visibly at
// the first request rather than silently reporting to nothing.
const Path = "/api/v1/inventory"

// # Changing this contract
//
// This file is the whole interface between two repositories. Within v1:
//
//   - Fields may be added. They must be optional, and must be omitted when the
//     sender does not know them, so that a new operator can report to an old
//     control plane.
//   - Fields may not be renamed, retyped, or removed. A rename is a new
//     contract wearing the old version number, and the two sides compile
//     against this same package precisely so that the compiler, not a console
//     cell that renders empty, catches it.
//   - Semantics may not change without a version bump. `deployment.ready` means
//     what it says today, which is that the engine answered a probe.
//
// Receivers must ignore fields they do not recognise. That direction has to
// work: during a rolling upgrade the operator is upgraded first as often as
// not, and a receiver that rejected unknown fields would take the whole
// cluster's inventory down until both halves were upgraded.

// Validate rejects a report the control plane cannot store.
func (r Report) Validate() error {
	if r.Cluster.Name == "" {
		return fmt.Errorf("cluster.name is required")
	}
	for i, d := range r.Deployments {
		if d.Namespace == "" || d.Name == "" {
			return fmt.Errorf("deployments[%d] needs both a namespace and a name", i)
		}
	}
	return nil
}
