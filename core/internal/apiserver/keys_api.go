package apiserver

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

type keyBody struct {
	ProjectID string `json:"projectId"`
	Label     string `json:"label"`
}

// createdKey carries the secret. It is in this response and nowhere else: only
// a hash is stored, so there is no second read of it, and a caller that loses
// it creates another key.
type createdKey struct {
	sqlstore.Key
	Secret string `json:"secret"`
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	rows, err := s.db.ListKeys(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var b keyBody
	if !decode(w, r, &b) {
		return
	}
	tenant, project := splitProject(b.ProjectID)
	if tenant == "" {
		writeError(w, errs.InvalidArgument("projectId must be tenant/name"))
		return
	}
	secret, id, err := s.keys.CreateKey(r.Context(), tenant, project, b.Label)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createdKey{
		Key: sqlstore.Key{
			ID: id, TenantID: tenant, ProjectID: b.ProjectID,
			Label: b.Label, Prefix: sqlstore.DisplayPrefix(secret),
		},
		Secret: secret,
	})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	if err := s.db.DeleteKey(r.Context(), chi.URLParam(r, "*")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type budgetRuleBody struct {
	ScopeKind string `json:"scopeKind"`
	ScopeID   string `json:"scopeId"`
	Dimension string `json:"dimension"`
	Limit     int64  `json:"limit"`
	Window    string `json:"window"`
}

type budgetRuleView struct {
	quota.Rule
	WindowText   string `json:"windowText"`
	WindowSecond int64  `json:"windowSeconds"`
}

func viewOf(rule quota.Rule) budgetRuleView {
	return budgetRuleView{Rule: rule, WindowText: rule.Window.String(), WindowSecond: rule.WindowSeconds()}
}

func (s *Server) listBudgetRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	q := r.URL.Query()
	id := q.Get("scopeId")
	if id == "" {
		writeError(w, errs.InvalidArgument("scopeId is required"))
		return
	}
	kinds := []quota.ScopeKind{quota.KindTenant, quota.KindProject}
	if k := quota.ScopeKind(q.Get("scopeKind")); k != "" {
		kinds = []quota.ScopeKind{k}
	}
	out := []budgetRuleView{}
	for _, kind := range kinds {
		rows, err := s.quota.Rules(r.Context(), kind, id)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		for _, rule := range rows {
			out = append(out, viewOf(rule))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putBudgetRule(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var b budgetRuleBody
	if !decode(w, r, &b) {
		return
	}
	win, err := quota.ParseDuration(b.Window)
	if err != nil {
		writeError(w, errs.InvalidArgument("window: %v", err))
		return
	}
	rule := quota.Rule{
		ScopeKind: quota.ScopeKind(b.ScopeKind),
		ScopeID:   b.ScopeID,
		Dimension: quota.Dimension(b.Dimension),
		Limit:     b.Limit,
		Window:    win,
	}
	if err := rule.Validate(); err != nil {
		writeError(w, errs.InvalidArgument("%v", err))
		return
	}
	if err := s.quota.PutRule(r.Context(), rule); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteBudgetRule takes the id "scopeKind/scopeId/dimension/window". The window
// has to be in it because a dimension may be capped over several windows and
// deleting one must not delete the others.
//
// The segments are taken from the ends rather than split into four, because a
// project scopeId is itself "tenant/name" and carries a slash of its own.
// Splitting positionally produced five segments for every project rule, so a
// project budget could be set and read but never removed — which is the one
// operation an operator needs when a cap turns out to be wrong.
func (s *Server) deleteBudgetRule(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	parts := strings.Split(chi.URLParam(r, "*"), "/")
	if len(parts) < 4 || parts[0] == "" || parts[len(parts)-2] == "" {
		writeError(w, errs.InvalidArgument(
			"id must be scopeKind/scopeId/dimension/window, for example "+
				"tenant/acme/tokens_total/5h or project/acme/research/tokens_total/5h"))
		return
	}
	kind := quota.ScopeKind(parts[0])
	scopeID := strings.Join(parts[1:len(parts)-2], "/")
	dimension := quota.Dimension(parts[len(parts)-2])
	win, err := quota.ParseDuration(parts[len(parts)-1])
	if err != nil {
		writeError(w, errs.InvalidArgument("window: %v", err))
		return
	}
	if err := s.quota.DeleteRule(r.Context(), kind, scopeID, dimension, win); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
