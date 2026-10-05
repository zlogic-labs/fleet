// Package registry holds what the control plane knows: which models exist,
// which pulls are running, and what the operator has reported about clusters.
package registry

import (
	"context"
	"errors"
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
	Tokenizer string    `json:"tokenizerId"`
	Context   int       `json:"contextLimit"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
