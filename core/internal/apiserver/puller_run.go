package apiserver

import (
	"context"
	"sync"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

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
	// The qualified name, not the bare one. Every entry in the registry is
	// keyed owner/name — startPull stores it that way and UpsertModel below
	// writes to it — so looking up `name` alone found nothing, ever, and prev
	// was silently the zero Model. That made the tokenizer carry-over below
	// dead code: the one place a real encoding name could be inherited from,
	// reading an empty string every time.
	//
	// The error is dropped deliberately: a first pull has no prior entry, and
	// there is nothing to report for one.
	prev, _ := p.Store.GetModel(ctx, owner+"/"+name)
	tokenizer := tokenizerFor(format, prev.Tokenizer)
	job.Commit = repo.Resolved
	job.Prefix = prefix.String()
	job.Format = string(format)
	job.FilesTotal = len(repo.Files)
	job.BytesTotal = repo.TotalBytes
	job.BytesDone = 0
	job.Progress = 0

	// Everything the worker sets on its own copy has to be published, because
	// the store took its copy before any of it was known. The console reads the
	// stored job, so a field set only on the worker's is a field the console
	// never sees.
	if _, err := p.Store.UpdatePull(ctx, job.ID, func(q *registry.Pull) {
		q.Commit, q.Prefix, q.Format = job.Commit, job.Prefix, job.Format
		q.FilesTotal, q.BytesTotal = job.FilesTotal, job.BytesTotal
		q.BytesDone, q.Progress = 0, 0
	}); err != nil {
		return err
	}

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
	// Total is fixed before the first file, so it is safe to read outside the
	// accumulator's lock.
	total := job.BytesTotal
	progress := func(file string, n int64) {
		mu.Lock()
		bytesDone += n
		filesDone++
		done, files := bytesDone, filesDone
		mu.Unlock()

		fraction := 0.0
		if total > 0 {
			fraction = float64(done) / float64(total)
		}
		// Both copies, deliberately. The worker's is what runOne publishes at
		// the end; the store's is what the console polls while the pull runs.
		// Writing only the worker's made the console show a job frozen at zero
		// until it finished, and writing the stored one directly raced every
		// poll -- which is what this replaced.
		job.BytesDone, job.FilesDone, job.CurrentFile, job.Progress = done, files, file, fraction
		_, _ = p.Store.UpdatePull(ctx, job.ID, func(q *registry.Pull) {
			q.BytesDone, q.FilesDone, q.CurrentFile, q.Progress = done, files, file, fraction
		})
	}

	if err := p.fetchAll(ctx, h, repo, prefix, progress); err != nil {
		return p.fail(ctx, job, owner+"/"+name, err)
	}

	mu.Lock()
	job.CurrentFile = ""
	job.Progress = 1
	mu.Unlock()
	_, _ = p.Store.UpdatePull(ctx, job.ID, func(q *registry.Pull) {
		q.CurrentFile = ""
		q.Progress = 1
	})

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
