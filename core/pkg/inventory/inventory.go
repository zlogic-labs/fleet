// Package inventory is the wire contract an operator reports to the control
// plane with.
//
// It is in pkg/ rather than inside the control plane's own registry because the
// operator has to speak it and the operator is a separate Go module: an
// internal package is unreachable from there, and a contract one side cannot
// name is a contract that quietly drifts. This is the same reason weights is a
// package — it is a seam two modules share, not a detail either one owns.
package inventory

import "time"

// GPU is one device on a node, or the summary of a homogeneous set.
type GPU struct {
	Model       string `json:"model"`
	Count       int    `json:"count"`
	TotalMemMiB int64  `json:"totalMemoryMiB"`
}

// Node is one reported node.
type Node struct {
	Name                 string            `json:"name"`
	Ready                bool              `json:"ready"`
	Roles                []string          `json:"roles"`
	GPU                  GPU               `json:"gpu"`
	CPUMillis            int64             `json:"cpuMillicores"`
	AllocatableMemoryMiB int64             `json:"allocatableMemoryMiB"`
	Kubelet              string            `json:"kubeletVersion"`
	OSImage              string            `json:"osImage"`
	Addresses            map[string]string `json:"addresses"`
	ReportedAt           time.Time         `json:"reportedAt"`
}

// Cluster is one operator's inventory report.
//
// The counts are fields rather than methods because the console reads them off
// the wire, and encoding/json does not call methods: a NodeCount() method
// serializes to nothing and the console renders three undefined cells.
type Cluster struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version"`
	// NodeCount, ReadyNodes, GPUCount and ReadyGPUs are recomputed on report,
	// so the summary and the node list can never disagree.
	NodeCount  int       `json:"nodeCount"`
	ReadyNodes int       `json:"readyNodes"`
	GPUCount   int       `json:"gpuCount"`
	ReadyGPUs  int       `json:"readyGpus"`
	CPUMillis  int64     `json:"cpuMillicores"`
	MemoryMiB  int64     `json:"memoryMiB"`
	Nodes      []Node    `json:"nodes"`
	ReportedAt time.Time `json:"reportedAt"`
	Message    string    `json:"message,omitempty"`
}

// Recompute derives the summary counts from the node list.
func (c *Cluster) Recompute() {
	c.NodeCount = len(c.Nodes)
	c.ReadyNodes = 0
	c.GPUCount = 0
	c.ReadyGPUs = 0
	for _, node := range c.Nodes {
		c.GPUCount += node.GPU.Count
		if node.Ready {
			c.ReadyNodes++
			c.ReadyGPUs += node.GPU.Count
		}
	}
}

// DeployState mirrors the FleetDeployment status phases rather than inventing a
// second vocabulary the operator would have to translate.
type DeployState string

const (
	DeployPending    DeployState = "Pending"
	DeployScheduling DeployState = "Scheduling"
	// DeployInsufficientCapacity is separate from Scheduling because the two
	// need different actions: one resolves on its own, the other needs the spec
	// changed. From outside a Deployment's status they look identical.
	DeployInsufficientCapacity DeployState = "InsufficientCapacity"
	DeployProgressing          DeployState = "Progressing"
	DeployAvailable            DeployState = "Available"
	DeployDegraded             DeployState = "Degraded"
	DeployFailed               DeployState = "Failed"
)

// Deployment is one reported FleetDeployment.
type Deployment struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Model     string `json:"model"`
	Desired   int    `json:"desiredReplicas"`
	Ready     int    `json:"readyReplicas"`
	TP        int    `json:"tensorParallelSize"`
	PP        int    `json:"pipelineParallelSize"`
	GPUPer    int    `json:"gpuPerReplica"`
	// Engine is the engine family the deployment asked for. It travels with
	// the report so the gateway can pick the right profile without guessing,
	// and so the console can say which engine a row is running rather than
	// inferring it from a name.
	Engine string      `json:"engine"`
	State  DeployState `json:"state"`
	Reason string      `json:"reason,omitempty"`
	// ObservedAt is stamped by the control plane on receipt, not by the operator:
	// what the console shows as an age is how long ago the fact arrived, which
	// is what an operator asking "is this still true" actually wants to know.
	ObservedAt time.Time `json:"updatedAt,omitempty"`
	// Selector is the model name clients address this deployment by.
	Selector string `json:"selector,omitempty"`
	// Address is the in-cluster endpoint, a Service DNS name rather than a
	// Pod IP so that it survives rescheduling. It is the one field the
	// gateway cannot work without: a deployment with no address is a promise
	// the operator has made and not yet kept.
	Address string `json:"address,omitempty"`
	// Version is what the engine reported about itself, which proves the
	// probe found a real runtime rather than assuming a version (P4).
	Version string `json:"version,omitempty"`
	// Format is the weight layout the model turned out to be.
	Format string `json:"format,omitempty"`
}

// Key is the namespace-qualified identity.
func (d Deployment) Key() string { return d.Namespace + "/" + d.Name }

// Report is the body an operator POSTs to /api/v1/inventory.
type Report struct {
	Cluster     Cluster      `json:"cluster"`
	Deployments []Deployment `json:"deployments"`
}
