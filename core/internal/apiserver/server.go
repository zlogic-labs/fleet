// Package apiserver is the control plane: the management API behind the
// console's Models and Cluster pages.
//
// It is a separate process from the gateway and a separate Go module concern,
// because it has a different failure domain. The gateway must stay up when a
// 140 GiB pull is saturating a link; the control plane can be busy, restarting
// and rebuilding for an hour without a single request being affected.
package apiserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Config is the control plane's configuration.
type Config struct {
	Listen string
	// Blobs is the object store. Nil in dev, where a directory is used.
	Blobs blobstore.Store
	// Hub is the model repository. Nil means the stub, which only has
	// synthetic repositories.
	Hub hub.Hub
	// Token authenticates to gated Hub repositories.
	Token string
	// PullConcurrency is how many pulls may run at once.
	PullConcurrency int
	// FileConcurrency is how many files within one pull may download at once.
	FileConcurrency int
	// AllowedOrigins is the CORS allowlist. Empty means loopback only.
	AllowedOrigins []string
	Version        string
	Edition        string
}

// Server owns the control plane's dependencies.
type Server struct {
	cfg    Config
	store  registry.Store
	blobs  blobstore.Store
	hub    hub.Hub
	puller *Puller
	worker *registry.Worker
	log    *slog.Logger
}

func NewServer(cfg Config, store registry.Store, log *slog.Logger) (*Server, error) {
	if store == nil {
		store = registry.NewMemory()
	}
	blobs := cfg.Blobs
	if blobs == nil {
		return nil, errs.Internal(errNoStore)
	}
	if err := blobs.EnsureBucket(context.Background()); err != nil {
		return nil, errs.Internal(err)
	}
	h := cfg.Hub
	if h == nil {
		h = hub.NewStub()
	}

	puller := NewPuller(store, blobs, h, log)
	puller.FileConcurrency = cfg.FileConcurrency
	worker := registry.NewWorker(store, cfg.PullConcurrency)
	worker.Start(puller.Run)

	return &Server{
		cfg: cfg, store: store, blobs: blobs, hub: h,
		puller: puller, worker: worker, log: log,
	}, nil
}

func (s *Server) Close() {
	// Drains the queue and lets running pulls finish; a 40 GiB pull should not
	// be abandoned because the control plane is restarting.
	s.worker.Close()
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(cors(s.cfg.AllowedOrigins), requestLog(s.log), recoverer(s.log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	r.Get("/readyz", s.ready)

	r.Route("/api/v1", func(r chi.Router) {
		// A wildcard, not a named parameter. A model name is owner/name, and
		// chi's {name} and {name:.+} both stop at the first slash, so
		// GET /models/Qwen/Qwen2.5 would 404 while
		// GET /models/Qwen%2FQwen2.5 would work. "*" takes the whole tail and
		// modelName unescapes it, so both forms land on the same entry.
		r.Get("/models", s.listModels)
		r.Get("/models/*", s.getModel)
		r.Delete("/models/*", s.deleteModel)
		// Verify lives under its own prefix rather than as /models/*/verify:
		// a wildcard cannot be followed by a fixed segment, and
		// /models/owner/verify would be ambiguous with a model named verify.
		r.Post("/verify/*", s.verifyModel)

		r.Post("/pulls", s.startPull)
		r.Get("/pulls", s.listPulls)
		r.Get("/pulls/{id}", s.getPull)
		r.Delete("/pulls/{id}", s.cancelPull)

		r.Get("/storage", s.storageInfo)

		// The operator reports here. Neither this nor the read side knows
		// what Kubernetes is; the operator is the only component that does.
		r.Post("/operator/inventory", s.reportInventory)
		r.Get("/cluster", s.clusterStatus)
		r.Get("/deployments", s.listDeployments)
	})

	return r
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	// Readiness fails when storage is unreachable. A control plane that cannot
	// reach its weights cannot start a pull, and a deployment it reports as
	// healthy will not be able to mount anything.
	info := s.blobs.Info(r.Context())
	if !info.Reachable {
		openai.WriteError(w, errs.Unavailable("object storage is not reachable: %s", info.Message))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ── models ─────────────────────────────────────────────────────

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.ListModels(r.Context())
	if err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	name := modelName(w, r)
	if name == "" {
		return
	}
	mdl, err := s.store.GetModel(r.Context(), name)
	if err != nil {
		openai.WriteError(w, errs.NotFound("no model named %q", name))
		return
	}
	writeJSON(w, http.StatusOK, mdl)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	name := modelName(w, r)
	if name == "" {
		return
	}
	if err := s.store.DeleteModel(r.Context(), name); err != nil {
		openai.WriteError(w, errs.NotFound("no model named %q", name))
		return
	}
	// Stored objects are deliberately left alone: the registry entry is a
	// pointer, and deleting 140 GiB on a DELETE is not what anyone means.
	s.log.Info("model deregistered", "model", name, "objects_retained", true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyModel(w http.ResponseWriter, r *http.Request) {
	name := modelName(w, r)
	if name == "" {
		return
	}
	found, missing, err := s.puller.Verify(r.Context(), name)
	if err != nil {
		openai.WriteError(w, errs.InvalidArgument("%s", err))
		return
	}
	// An empty list, not null: the console renders `missing` directly, and
	// null reads as "the check did not run".
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": found,
		"missing": missing,
		"ok":      len(missing) == 0,
	})
}

// modelName reads the wildcard tail, unescaped.
//
// A model name is owner/name. A client that percent-encodes the slash sends
// %2F and one that does not sends a real slash; both are accepted, because
// requiring the encoding makes the API work from a browser fetch and not from
// a shell. Answering 400 on a malformed escape rather than falling through to
// a 404 keeps "you sent nonsense" distinguishable from "no such model".
func modelName(w http.ResponseWriter, r *http.Request) string {
	raw := chi.URLParam(r, "*")
	name, err := url.PathUnescape(raw)
	if err != nil {
		openai.WriteError(w, errs.InvalidArgument("model name is not valid percent-encoding: %q", raw))
		return ""
	}
	if name == "" {
		openai.WriteError(w, errs.InvalidArgument("a model name is required"))
		return ""
	}
	return name
}

// ── pulls ──────────────────────────────────────────────────────

type startPullRequest struct {
	Model        string `json:"model"`
	Source       string `json:"source"`
	SourceRef    string `json:"sourceRef"`
	Revision     string `json:"revision"`
	TokenizerID  string `json:"tokenizerId"`
	ContextLimit int    `json:"contextLimit"`
	HFToken      string `json:"hfToken"`
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

// ── storage ────────────────────────────────────────────────────

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.blobs.Info(r.Context()))
}

// ── operator reports ───────────────────────────────────────────

func (s *Server) reportInventory(w http.ResponseWriter, r *http.Request) {
	var report struct {
		Cluster     registry.Cluster      `json:"cluster"`
		Deployments []registry.Deployment `json:"deployments"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&report); err != nil {
		openai.WriteError(w, errs.InvalidArgument("body is not an inventory report: %s", err))
		return
	}
	if report.Cluster.Name == "" {
		openai.WriteError(w, errs.InvalidArgument("cluster.name is required"))
		return
	}
	if err := s.store.ReportCluster(r.Context(), report.Cluster); err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	for _, d := range report.Deployments {
		if err := s.store.UpsertDeployment(r.Context(), d); err != nil {
			openai.WriteError(w, errs.Internal(err))
			return
		}
	}
	s.log.Info("inventory reported",
		"cluster", report.Cluster.Name,
		"nodes", report.Cluster.NodeCount,
		"gpus", report.Cluster.GPUCount,
		"deployments", len(report.Deployments))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clusterStatus(w http.ResponseWriter, r *http.Request) {
	clusters, err := s.store.ListClusters(r.Context())
	if err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	// A non-nil empty slice, not nil: the console distinguishes "no operator
	// has reported" from "the field is missing", and JSON null would collapse
	// the two.
	if clusters == nil {
		clusters = []registry.Cluster{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": clusters})
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	deps, err := s.store.ListDeployments(r.Context())
	if err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	if deps == nil {
		deps = []registry.Deployment{}
	}
	writeJSON(w, http.StatusOK, deps)
}

// ── plumbing ───────────────────────────────────────────────────

var errNoStore = &noStoreError{}

type noStoreError struct{}

func (*noStoreError) Error() string {
	return "no object store configured; the control plane has nowhere to put weights"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", strconv.FormatInt(rec.bytes, 10),
				"duration_ms", time.Since(start).Milliseconds())
		})
	}
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "path", r.URL.Path, "panic", v)
					openai.WriteError(w, errs.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
