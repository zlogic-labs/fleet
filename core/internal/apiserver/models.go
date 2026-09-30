package apiserver

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

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
	// The caller states the engine rather than having one assumed, and the
	// response echoes it back so the console shows which check actually ran.
	// The default is vllm only because it is the common case, not because it
	// is a good default for a GGUF model.
	engineName := r.URL.Query().Get("engine")
	if engineName == "" {
		engineName = "vllm"
	}
	found, missing, err := s.puller.Verify(r.Context(), name, engineName)
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
		"engine":  engineName,
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
