package registry

import "github.com/zlogic-labs/fleet/core/pkg/inventory"

// The cluster inventory types live in core/pkg/inventory rather than here,
// because the operator has to produce them and an internal package is
// unreachable from the operator module. These aliases keep every existing
// reader of registry.Cluster working while pointing a type error at the real
// home.
type (
	Cluster     = inventory.Cluster
	Node        = inventory.Node
	GPU         = inventory.GPU
	Deployment  = inventory.Deployment
	DeployState = inventory.DeployState
)

const (
	DeployPending              = inventory.DeployPending
	DeployScheduling           = inventory.DeployScheduling
	DeployInsufficientCapacity = inventory.DeployInsufficientCapacity
	DeployProgressing          = inventory.DeployProgressing
	DeployAvailable            = inventory.DeployAvailable
	DeployDegraded             = inventory.DeployDegraded
	DeployFailed               = inventory.DeployFailed
)
