package apiserver

import (
	"encoding/json"
	"net/http"

	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

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
