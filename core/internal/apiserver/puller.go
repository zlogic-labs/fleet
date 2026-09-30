package apiserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/engine"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
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
	tokenizer := tokenizerFor(format, job.Model)
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

// fetchAll downloads every file, a few at a time.
//
// Each file is streamed straight into storage rather than buffered: a 4 GiB
// shard does not fit in the memory a control plane should be using, and
// buffering it here would make the failure mode "the pod gets OOM-killed at
// 80% of a pull" instead of a resumable, reportable error.
func (p *Puller) fetchAll(ctx context.Context, h hub.Hub, repo hub.Repo, prefix blobstore.ModelPrefix, progress func(string, int64)) error {
	limit := p.FileConcurrency
	if limit < 1 {
		limit = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(repo.Files))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for _, file := range repo.Files {
		select {
		case <-ctx.Done():
			// Stop launching new work but let what is running settle, so the
			// error reported is the real one and not a derived cancellation.
			wg.Wait()
			return firstErr(errs)
		default:
		}

		file := file
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := p.fetchOne(ctx, h, prefix, file); err != nil {
				errs <- err
				cancel()
				return
			}
			progress(file.Path, file.Size)
		}()
	}

	wg.Wait()
	return firstErr(errs)
}

func (p *Puller) fetchOne(ctx context.Context, h hub.Hub, prefix blobstore.ModelPrefix, file hub.File) error {
	body, _, err := h.Open(ctx, file)
	if err != nil {
		return fmt.Errorf("%s: %w", file.Path, err)
	}
	defer body.Close()

	key := prefix.Key(file.Path)
	if _, err := p.Blobs.Put(ctx, key, body, file.Size); err != nil {
		return fmt.Errorf("%s: storing: %w", file.Path, err)
	}
	return nil
}

func firstErr(errs chan error) error {
	for {
		select {
		case err := <-errs:
			if err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// Verify checks that the stored objects satisfy the engine that will load
// them.
//
// A registry entry in Ready state is a claim, and the claim is only worth
// something if a deployment can rely on it. This is what an operator runs when
// a pod fails to load weights and the question is whether the pull lied or the
// mount is wrong.
//
// The engine argument is what makes this check meaningful. Verifying against a
// hardcoded safetensors file list marks every GGUF model as incomplete, and a
// false "incomplete" on a model that is fine sends an operator to re-download
// several gigabytes that were already correct.
func (p *Puller) Verify(ctx context.Context, name, engineName string) (found int, missing []string, err error) {
	mdl, err := p.Store.GetModel(ctx, name)
	if err != nil {
		return 0, nil, err
	}
	if mdl.State != registry.StateReady {
		return 0, nil, fmt.Errorf("model %s is %s, not ready", name, mdl.State)
	}

	if err := engine.Compatible(mdl.Format, engineName); err != nil {
		return 0, nil, fmt.Errorf("%s: %w", name, err)
	}
	prof := p.Profiles.For(engineName)

	objs, err := p.Blobs.List(ctx, mdl.Prefix, 0)
	if err != nil {
		return 0, nil, err
	}
	rel := make([]string, 0, len(objs))
	for _, o := range objs {
		rel = append(rel, strings.TrimPrefix(o.Key, mdl.Prefix+"/"))
	}
	return len(rel), prof.VerifyFiles(rel), nil
}

// tokenizerFor returns the tokenizer id to record for a model.
//
// A GGUF carries its tokenizer inside the weights, so naming an external one
// is a claim Fleet cannot check and the engine will ignore. Returning the model
// name is the same claim with an extra step. The honest answer is empty: the
// gateway's own resolver will find the closest matching encoding, and the
// console shows that as "embedded".
func tokenizerFor(f weights.Format, fallback string) string {
	if f == weights.GGUF {
		return ""
	}
	return fallback
}

func paths(repo hub.Repo) []string {
	out := make([]string, 0, len(repo.Files))
	for _, f := range repo.Files {
		out = append(out, f.Path)
	}
	return out
}
