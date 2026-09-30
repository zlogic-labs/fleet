// Package registry holds what the control plane knows: which models exist,
// which pulls are running, and what the operator has reported about clusters.
package registry

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// ErrNotFound is returned for a name that is not registered.
var ErrNotFound = errors.New("registry: not found")

// ModelState is where a model is in its lifecycle.
//
// The states are ordered by how much the operator can rely on the weights
// being present: only Ready means a deployment can mount the prefix and expect
// a complete set of files.
type ModelState string

const (
	// StatePending is registered but no pull has been started.
	StatePending ModelState = "pending"
	// StatePulling means objects are being written right now.
	StatePulling ModelState = "pulling"
	// StateReady means every file of the revision is in storage.
	StateReady ModelState = "ready"
	// StateFailed means the last pull failed; Message says why.
	StateFailed ModelState = "failed"
)

// Model is one entry in the registry.
type Model struct {
	Name      string     `json:"name"`
	Source    string     `json:"source"`
	SourceRef string     `json:"sourceRef"`
	Revision  string     `json:"revision"`
	Commit    string     `json:"commit"`
	Prefix    string     `json:"storagePrefix"`
	SizeBytes int64      `json:"sizeBytes"`
	Files     int        `json:"files"`
	State     ModelState `json:"state"`
	// Format is how the bytes are laid out: safetensors or gguf. It decides
	// which engines can load the model, so it is part of the entry's identity
	// rather than a detail of how it was fetched.
	Format weights.Format `json:"format"`
	// Tokenizer names the tokenizer to use for local token counting. It is
	// empty for GGUF, where the tokenizer is embedded in the weights and a
	// separate id would be a fiction.
	Tokenizer  string    `json:"tokenizerId"`
	Context    int       `json:"contextLimit"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	ReportedAt time.Time `json:"reportedAt,omitempty"`
}

// PullState is a job's lifecycle.
type PullState string

const (
	PullQueued   PullState = "queued"
	PullRunning  PullState = "running"
	PullDone     PullState = "done"
	PullFailed   PullState = "failed"
	PullCanceled PullState = "canceled"
)

// Running reports whether a pull still occupies a worker.
func (s PullState) Running() bool { return s == PullQueued || s == PullRunning }

// Pull is one download job.
type Pull struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	SourceRef string `json:"sourceRef"`
	Revision  string `json:"revision"`
	Commit    string `json:"commit"`
	Prefix    string `json:"storagePrefix"`
	// Format is the weight layout the repository turned out to be, resolved
	// from the files rather than requested.
	Format string    `json:"format"`
	State  PullState `json:"state"`

	// Progress is a fraction in [0,1] over bytes, computed from the Hub's
	// declared total rather than from a running total that starts at zero and
	// therefore jumps around.
	Progress    float64 `json:"progress"`
	BytesDone   int64   `json:"bytesDone"`
	BytesTotal  int64   `json:"bytesTotal"`
	FilesDone   int     `json:"filesDone"`
	FilesTotal  int     `json:"filesTotal"`
	CurrentFile string  `json:"currentFile"`
	Error       string  `json:"error,omitempty"`

	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	// Token authenticates to a gated repository. It is a password: it is
	// never serialized (every write path redacts it) and never logged.
	Token string `json:"-"`

	// done is closed on completion so a waiting caller wakes without polling.
	done chan struct{}
	// cancel stops the worker.
	cancel context.CancelFunc
}

// Done exposes the completion channel.
func (p *Pull) Done() <-chan struct{} { return p.done }

// Store is the registry backend.
//
// The interface is small and synchronous on purpose: every method is a
// metadata operation on a small amount of data, and the only large thing in
// the system — the weights — is in blob storage, not here.
type Store interface {
	UpsertModel(ctx context.Context, m Model) (Model, error)
	GetModel(ctx context.Context, name string) (Model, error)
	ListModels(ctx context.Context) ([]Model, error)
	DeleteModel(ctx context.Context, name string) error

	CreatePull(ctx context.Context, p *Pull) error
	UpdatePull(ctx context.Context, id string, fn func(*Pull)) (*Pull, error)
	GetPull(ctx context.Context, id string) (*Pull, error)
	ListPulls(ctx context.Context) ([]*Pull, error)
	// CancelPull stops a running job. It reports false when the job was
	// already finished.
	CancelPull(ctx context.Context, id string) (bool, error)

	ReportCluster(ctx context.Context, c Cluster) error
	ListClusters(ctx context.Context) ([]Cluster, error)
	UpsertDeployment(ctx context.Context, d Deployment) error
	ListDeployments(ctx context.Context) ([]Deployment, error)
}

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

// Memory is the in-process Store.
//
// It is the default backend, and the right one for a single control plane:
// the registry is metadata, a Fleet installation has a handful of models, and
// a pull in flight is a process the operator can see. Postgres arrives when
// there is more than one control plane, which is also when the high-availability
// capability becomes worth buying.
type Memory struct {
	mu          sync.RWMutex
	models      map[string]Model
	pulls       map[string]*Pull
	order       []string
	clusters    map[string]Cluster
	deployments map[string]Deployment
}

var _ Store = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{
		models:      map[string]Model{},
		pulls:       map[string]*Pull{},
		clusters:    map[string]Cluster{},
		deployments: map[string]Deployment{},
	}
}

func (m *Memory) UpsertModel(_ context.Context, in Model) (Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	prev, existed := m.models[in.Name]
	out := in
	if existed {
		out.CreatedAt = prev.CreatedAt
		// A partial update must not blank fields it does not mention. Every
		// caller here updates a subset, so a zero means "not supplied" and the
		// previous value stands — including State, because silently un-readying
		// a model whose weights are already stored would force another pull.
		// Zero is safe to treat as absent for all of these: a model with a
		// context limit of 0 or a size of 0 bytes is not a thing that exists.
		inherit(&out.State, prev.State)
		inherit(&out.SizeBytes, prev.SizeBytes)
		inherit(&out.Files, prev.Files)
		inherit(&out.Prefix, prev.Prefix)
		inherit(&out.Commit, prev.Commit)
		inherit(&out.Tokenizer, prev.Tokenizer)
		inherit(&out.Context, prev.Context)
		inherit(&out.Format, prev.Format)
	}
	out.UpdatedAt = now
	if out.CreatedAt.IsZero() {
		out.CreatedAt = now
	}
	m.models[in.Name] = out
	return out, nil
}

func (m *Memory) GetModel(_ context.Context, name string) (Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mdl, ok := m.models[name]
	if !ok {
		return Model{}, ErrNotFound
	}
	return mdl, nil
}

func (m *Memory) ListModels(_ context.Context) ([]Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Model, 0, len(m.models))
	for _, mdl := range m.models {
		out = append(out, mdl)
	}
	sortModels(out)
	return out, nil
}

func (m *Memory) DeleteModel(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.models[name]; !ok {
		return ErrNotFound
	}
	// The objects stay. A registry entry is a pointer at weights, and deleting
	// a pointer is cheap while deleting 140 GiB on a mis-click is not.
	delete(m.models, name)
	return nil
}

func (m *Memory) CreatePull(_ context.Context, p *Pull) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pulls[p.ID]; ok {
		return errors.New("registry: pull already exists: " + p.ID)
	}
	if p.done == nil {
		p.done = make(chan struct{})
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = time.Now().UTC()
	}
	m.pulls[p.ID] = p
	m.order = append(m.order, p.ID)
	return nil
}

func (m *Memory) UpdatePull(_ context.Context, id string, fn func(*Pull)) (*Pull, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pulls[id]
	if !ok {
		return nil, ErrNotFound
	}
	fn(p)
	return p, nil
}

func (m *Memory) GetPull(_ context.Context, id string) (*Pull, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pulls[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p.clone(), nil
}

func (m *Memory) ListPulls(_ context.Context) ([]*Pull, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Pull, 0, len(m.pulls))
	for _, id := range m.order {
		if p, ok := m.pulls[id]; ok {
			out = append(out, p.clone())
		}
	}
	return out, nil
}

func (m *Memory) CancelPull(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pulls[id]
	if !ok {
		return false, ErrNotFound
	}
	if !p.State.Running() {
		return false, nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	return true, nil
}

func (m *Memory) ReportCluster(_ context.Context, c Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.ReportedAt = time.Now().UTC()
	// Recomputed here rather than in the operator, so a hand-written or
	// replayed report cannot lie about its own totals.
	c.Recompute()
	m.clusters[c.Name] = c
	return nil
}

func (m *Memory) ListClusters(_ context.Context) ([]Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c)
	}
	sortClusters(out)
	return out, nil
}

func (m *Memory) UpsertDeployment(_ context.Context, d Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.ObservedAt = time.Now().UTC()
	m.deployments[d.Key()] = d
	return nil
}

func (m *Memory) ListDeployments(_ context.Context) ([]Deployment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Deployment, 0, len(m.deployments))
	for _, d := range m.deployments {
		out = append(out, d)
	}
	sortDeployments(out)
	return out, nil
}

// inherit keeps a previous value when the new one is the zero value.
func inherit[T comparable](dst *T, prev T) {
	var zero T
	if *dst == zero {
		*dst = prev
	}
}

// clone copies a pull for a reader. The live job is mutated by its worker
// without the store's lock held, so handing out the pointer would be a data
// race with the progress the console is polling.
func (p *Pull) clone() *Pull {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// AttachCancel wires the worker's cancel function to a stored job.
func (p *Pull) AttachCancel(fn context.CancelFunc) { p.cancel = fn }

// Finish releases anyone waiting on Done and stamps the end time. It is
// idempotent, so a worker that reports failure after cancellation does not
// panic on a second close.
func (p *Pull) Finish(state PullState, errMsg string) {
	p.State = state
	p.Error = errMsg
	t := time.Now().UTC()
	p.FinishedAt = &t
	if state == PullDone {
		p.Progress = 1
		p.BytesDone = p.BytesTotal
		p.FilesDone = p.FilesTotal
	}
	if p.done != nil {
		close(p.done)
		p.done = nil
	}
}
