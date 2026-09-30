package apiserver

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// ── pulls ──────────────────────────────────────────────────────

type startPullRequest struct {
	Model        string `json:"model"`
	Source       string `json:"source"`
	SourceRef    string `json:"sourceRef"`
	Revision     string `json:"revision"`
	TokenizerID  string `json:"tokenizerId"`
	ContextLimit int    `json:"contextLimit"`
	HFToken      string `json:"hfToken"`
	// Engine is optional. It does not change what gets pulled — the
	// repository's format is inferred from its files — but it is recorded so
	// that Verify has a target and the console can say which engines will be
	// able to load the result.
	Engine string `json:"engine"`
}

func (s *Server) startPull(w http.ResponseWriter, r *http.Request) {
	var body startPullRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		openai.WriteError(w, errs.InvalidArgument("body is not a pull request: %s", err))
		return
	}
	if body.SourceRef == "" {
		body.SourceRef = body.Model
	}
	owner, name, err := registry.NormalizeModelName(body.SourceRef)
	if err != nil {
		openai.WriteError(w, errs.InvalidArgument("%s", err))
		return
	}
	revision := body.Revision
	if revision == "" {
		revision = "main"
	}

	job := &registry.Pull{
		ID:        s.worker.NextID(),
		Model:     owner + "/" + name,
		Source:    "huggingface",
		SourceRef: owner + "/" + name,
		Revision:  revision,
		State:     registry.PullQueued,
		// Carried on the job, never on the server, so a token for one gated
		// repository is not retained once the job is done.
		Token: body.HFToken,
	}
	if _, err := s.store.UpsertModel(r.Context(), registry.Model{
		Name:      job.Model,
		Source:    "huggingface",
		SourceRef: job.SourceRef,
		Revision:  job.Revision,
		State:     registry.StatePending,
		Tokenizer: body.TokenizerID,
		Context:   body.ContextLimit,
		Message:   "queued",
	}); err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	if err := s.worker.Enqueue(job); err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	s.log.Info("pull queued",
		"id", job.ID, "model", job.Model, "revision", revision,
		"gated", body.HFToken != "")
	writeJSON(w, http.StatusAccepted, redactToken(job))
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	pulls, err := s.store.ListPulls(r.Context())
	if err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	// Newest first: the console shows a running job at the top, and the list
	// is capped so a long-lived control plane does not accumulate thousands.
	if len(pulls) > 50 {
		pulls = pulls[len(pulls)-50:]
	}
	out := make([]*registry.Pull, len(pulls))
	for i := len(pulls) - 1; i >= 0; i-- {
		out[len(pulls)-1-i] = redactToken(pulls[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetPull(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		openai.WriteError(w, errs.NotFound("no pull with id %q", chi.URLParam(r, "id")))
		return
	}
	writeJSON(w, http.StatusOK, redactToken(job))
}

// redactToken strips the Hub token from any pull that is about to be
// serialized. A token is a password; the console never needs to see it back
// and a GET that returns it puts it in every browser's network log.
func redactToken(p *registry.Pull) *registry.Pull {
	cp := *p
	cp.Token = ""
	return &cp
}

func (s *Server) cancelPull(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	stopped, err := s.store.CancelPull(r.Context(), id)
	if err != nil {
		openai.WriteError(w, errs.NotFound("no pull with id %q", id))
		return
	}
	if !stopped {
		openai.WriteError(w, errs.InvalidArgument("pull %s has already finished", id))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
