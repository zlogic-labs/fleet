package apiserver

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// Puller runs one pull job: resolve the revision, then stream every file into
// object storage. Run lives in puller_run.go.
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

// DefaultFileConcurrency is how many files within one pull download at once
// when the operator expressed no preference. NewServer overwrites it with the
// configured value; the default exists so a Puller built directly is still
// usable rather than serial.
const DefaultFileConcurrency = 3

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
		FileConcurrency: DefaultFileConcurrency,
	}
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
