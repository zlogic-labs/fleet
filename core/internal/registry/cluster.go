package registry

import (
	"time"
)

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

// DeployState mirrors the FleetDeployment status conditions rather than
// inventing a second vocabulary the operator would have to translate.
type DeployState string

const (
	DeployPending     DeployState = "Pending"
	DeployScheduling  DeployState = "Scheduling"
	DeployProgressing DeployState = "Progressing"
	DeployAvailable   DeployState = "Available"
	DeployDegraded    DeployState = "Degraded"
	DeployFailed      DeployState = "Failed"
)

// Deployment is one reported FleetDeployment.
type Deployment struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Model      string      `json:"model"`
	Desired    int         `json:"desiredReplicas"`
	Ready      int         `json:"readyReplicas"`
	TP         int         `json:"tensorParallelSize"`
	PP         int         `json:"pipelineParallelSize"`
	GPUPer     int         `json:"gpuPerReplica"`
	State      DeployState `json:"state"`
	Reason     string      `json:"reason,omitempty"`
	ObservedAt time.Time   `json:"updatedAt"`
}

// Key is the namespace-qualified identity.
func (d Deployment) Key() string { return d.Namespace + "/" + d.Name }
