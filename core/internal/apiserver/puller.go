package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// Puller runs one pull job: resolve the revision, then stream every file into
// object storage.
//
// The whole job writes into a prefix that includes the resolved commit, not
// the branch that was asked for. A deployment that already mounts main@abc123
// keeps working when a later pull of main resolves to def456, and a half
// finished pull is never visible to a deployment because the prefix it writes
// to does not exist until the first file lands there.
type Puller struct {
	Store registry.Store
	Blobs blobstore.Store
	Hub   hub.Hub
	Log   *slog.Logger
	// Profiles maps an engine family to what it can load. Verification needs
	// it, because "are these weights complete" has no answer without knowing
	// who is going to load them: a safetensors repository is missing
	// config.json, and a GGUF repository has no such expectation.
	Profiles *engine.Profiles
	// FileConcurrency bounds simultaneous file downloads within one job. The
	// worker bounds jobs; this bounds the files inside one.
	FileConcurrency int
}

func NewPuller(store registry.Store, blobs blobstore.Store, h hub.Hub,
	profiles *engine.Profiles, log *slog.Logger) *Puller {
	if profiles == nil {
		profiles = engine.BuiltinProfiles()
	}
	return &Puller{
		Store:           store,
		Blobs:           blobs,
		Hub:             h,
		Profiles:        profiles,
		Log:             log,
		FileConcurrency: 3,
	}
}

// Run executes a pull. It is the function the worker calls.
func (p *Puller) Run(ctx context.Context, job *registry.Pull) error {
	owner, name, err := registry.NormalizeModelName(job.SourceRef)
	if err != nil {
		return err
	}
	h := p.hubFor(job)
	// The token is cleared as soon as the job has a Hub, so a job sitting in
	// the store while it runs does not hold a password.
	job.Token = ""

	p.Log.Info("pull started", "id", job.ID, "model", job.SourceRef, "revision", job.Revision)
	// No tokenizer here. The repository has not been resolved yet, so the
	// format is not known, and UpsertModel treats a zero field as "not
	// provided" and inherits whatever is stored — a guess made now would
	// outlive the answer and survive the real one.
	if _, err := p.Store.UpsertModel(ctx, registry.Model{
		Name:      owner + "/" + name,
		Source:    "huggingface",
		SourceRef: owner + "/" + name,
		Revision:  job.Revision,
		State:     registry.StatePulling,
		Prefix:    blobstore.Prefix(owner, name, job.Revision).String(),
		Message:   "pulling",
	}); err != nil {
		return err
	}

	repo, err := h.Resolve(ctx, owner, name, job.Revision)
	if err != nil {
		return p.fail(ctx, job, owner+"/"+name, err)
	}

	// The commit, not the branch, is what gets stored. Everything downstream
	// pins to it.
	prefix := blobstore.Prefix(owner, name, repo.Resolved)
	format := weights.Of(paths(repo))
	// Whatever the operator asked for at queue time, and nothing else. The
	// entry written when the pull was queued is the only place a real
	// encoding name can come from; inventing one here would override it with
	// a guess that the gateway cannot use.
	prev, _ := p.Store.GetModel(ctx, name)
	tokenizer := tokenizerFor(format, prev.Tokenizer)
	job.Commit = repo.Resolved
	job.Prefix = prefix.String()
	job.Format = string(format)
	job.FilesTotal = len(repo.Files)
	job.BytesTotal = repo.TotalBytes
	job.BytesDone = 0
	job.Progress = 0

	// The tokenizer is set from the format here, not only on completion.
	// UpsertModel treats a zero field as "not provided" and inherits the
	// stored value, so a GGUF model that should have no tokenizer id would
	// get the repository name back on the finishing update and keep it.
	if _, err := p.Store.UpsertModel(ctx, registry.Model{
		Name:      owner + "/" + name,
		Source:    "huggingface",
		SourceRef: owner + "/" + name,
		Revision:  job.Revision,
		Commit:    repo.Resolved,
		Prefix:    prefix.String(),
		Tokenizer: tokenizer,
		Format:    format,
		State:     registry.StatePulling,
		Message:   "pulling",
	}); err != nil {
		return err
	}

	var (
		mu        sync.Mutex
		bytesDone int64
		filesDone int
	)
	progress := func(file string, n int64) {
		mu.Lock()
		defer mu.Unlock()
		bytesDone += n
		filesDone++
		job.BytesDone = bytesDone
		job.FilesDone = filesDone
		job.CurrentFile = file
		if job.BytesTotal > 0 {
			job.Progress = float64(bytesDone) / float64(job.BytesTotal)
		}
	}

	if err := p.fetchAll(ctx, h, repo, prefix, progress); err != nil {
		return p.fail(ctx, job, owner+"/"+name, err)
	}

	mu.Lock()
	job.CurrentFile = ""
	job.Progress = 1
	mu.Unlock()

	if _, err := p.Store.UpsertModel(ctx, registry.Model{
		Name:      owner + "/" + name,
		Source:    "huggingface",
		SourceRef: owner + "/" + name,
		Revision:  job.Revision,
		Commit:    repo.Resolved,
		Prefix:    prefix.String(),
		State:     registry.StateReady,
		Tokenizer: tokenizer,
		Format:    format,
		SizeBytes: repo.TotalBytes,
		Files:     len(repo.Files),
		Message:   "",
	}); err != nil {
		return err
	}

	// Older pulls of the same model at a different commit are now dead weight,
	// but they are not deleted: another deployment may still mount one. Garbage
	// collection of unreferenced prefixes is a deliberate, separate operation.
	p.Log.Info("pull finished",
		"id", job.ID, "model", owner+"/"+name, "commit", repo.Resolved,
		"files", len(repo.Files), "bytes", repo.TotalBytes)
	return nil
}

func (p *Puller) fail(ctx context.Context, job *registry.Pull, name string, cause error) error {
	// A cancel is not a failure. Marking the model failed would tell the
	// operator their repository is broken when the truth is that they asked to
	// stop; pending is the state that invites a retry.
	if errors.Is(cause, context.Canceled) {
		p.Log.Info("pull canceled", "id", job.ID, "model", name)
		_, _ = p.Store.UpsertModel(ctx, registry.Model{
			Name:      name,
			Source:    "huggingface",
			SourceRef: job.SourceRef,
			Revision:  job.Revision,
			State:     registry.StatePending,
			Message:   "canceled; pull again to retry",
		})
		return cause
	}

	p.Log.Error("pull failed", "id", job.ID, "model", name, "error", cause)
	_, _ = p.Store.UpsertModel(ctx, registry.Model{
		Name:      name,
		Source:    "huggingface",
		SourceRef: job.SourceRef,
		Revision:  job.Revision,
		Commit:    job.Commit,
		Prefix:    job.Prefix,
		State:     registry.StateFailed,
		Message:   cause.Error(),
	})
	return cause
}

// hubFor returns the Hub to use for a job, cloning the shared HTTP client when
// the job carries a token. The clone shares the transport, so it does not
// open a second connection pool.
func (p *Puller) hubFor(job *registry.Pull) hub.Hub {
	if job.Token == "" {
		return p.Hub
	}
	remote, ok := p.Hub.(*hub.HTTP)
	if !ok {
		return p.Hub
	}
	clone := *remote
	clone.Token = job.Token
	return &clone
}
