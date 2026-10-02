package apiserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

type tenantBody struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RequestLimit int64  `json:"requestLimit"`
	TokenLimit   int64  `json:"tokenLimit"`
	Active       *bool  `json:"active"`
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	rows, err := s.db.ListTenants(r.Context())
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(rows))
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	t, err := s.db.GetTenant(r.Context(), chi.URLParam(r, "*"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var b tenantBody
	if !decode(w, r, &b) {
		return
	}
	if b.ID == "" || b.Name == "" {
		writeError(w, errs.InvalidArgument("id and name are both required"))
		return
	}
	err := s.policies.CreateTenant(r.Context(), sqlstore.TenantRow{
		ID: b.ID, Name: b.Name,
		RequestLimit: int(b.RequestLimit), TokenLimit: int(b.TokenLimit), Active: true,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// updateTenant is a PATCH that reads as a partial edit but behaves as a
// replace of the fields it carries: absent fields inherit the current value
// rather than becoming zero. Setting a limit back to zero is still possible,
// because zero is a value and absence is not.
func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	id := chi.URLParam(r, "*")
	prev, err := s.db.GetTenant(r.Context(), id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var b tenantBody
	if !decode(w, r, &b) {
		return
	}
	active := prev.Active
	if b.Active != nil {
		active = *b.Active
	}
	if b.Name == "" {
		b.Name = prev.Name
	}
	err = s.db.UpdateTenant(r.Context(), id, b.Name,
		or(b.RequestLimit, prev.RequestLimit), or(b.TokenLimit, prev.TokenLimit), active)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	if err := s.db.DeleteTenant(r.Context(), chi.URLParam(r, "*")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type projectBody struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenantId"`
	Name         string `json:"name"`
	RequestLimit int64  `json:"requestLimit"`
	TokenLimit   int64  `json:"tokenLimit"`
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	rows, err := s.db.ListProjects(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	p, err := s.db.GetProject(r.Context(), chi.URLParam(r, "*"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var b projectBody
	if !decode(w, r, &b) {
		return
	}
	if b.TenantID == "" || b.Name == "" {
		writeError(w, errs.InvalidArgument("tenantId and name are both required"))
		return
	}
	if b.ID == "" {
		b.ID = b.TenantID + "/" + b.Name
	}
	err := s.policies.CreateProject(r.Context(), sqlstore.ProjectRow{
		ID: b.ID, TenantID: b.TenantID, Name: b.Name,
		RequestLimit: int(b.RequestLimit), TokenLimit: int(b.TokenLimit),
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	id := chi.URLParam(r, "*")
	prev, err := s.db.GetProject(r.Context(), id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var b projectBody
	if !decode(w, r, &b) {
		return
	}
	if b.Name == "" {
		b.Name = prev.Name
	}
	err = s.db.UpdateProject(r.Context(), id, b.Name,
		or(b.RequestLimit, prev.RequestLimit), or(b.TokenLimit, prev.TokenLimit))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	if err := s.db.DeleteProject(r.Context(), chi.URLParam(r, "*")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func or(next, prev int64) int64 {
	if next != 0 {
		return next
	}
	return prev
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
