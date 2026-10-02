package apiserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// ── operator reports ───────────────────────────────────────────

func (s *Server) reportInventory(w http.ResponseWriter, r *http.Request) {
	// Decoded into the shared contract type rather than a re-declared struct,
	// so the shape on the wire has exactly one definition in the tree — the one
	// fleet-serving also compiles against.
	var report inventory.Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&report); err != nil {
		openai.WriteError(w, errs.InvalidArgument("body is not an inventory report: %s", err))
		return
	}
	if err := report.Validate(); err != nil {
		openai.WriteError(w, errs.InvalidArgument("%s", err))
		return
	}
	// Version skew is worth a line in the log and nothing more. An operator
	// newer than this build has sent fields that were ignored; an older one has
	// sent fields that do not exist. Neither is a reason to drop a cluster's
	// entire inventory on the floor, and refusing is what would make the
	// upgrade order mandatory.
	if report.Contract != inventory.ContractVersion {
		s.log.Warn("inventory from a different contract version",
			"cluster", report.Cluster.Name,
			"got", report.Contract, "want", inventory.ContractVersion)
	}
	// The operator does not stamp the report time, and Memory.ReportCluster
	// stamps a copy the caller cannot see. Without this the capacity sample has
	// no timestamp and the cost pool has no period to integrate over.
	if report.Cluster.ReportedAt.IsZero() {
		report.Cluster.ReportedAt = time.Now().UTC()
	}
	if err := s.store.ReportCluster(r.Context(), report.Cluster); err != nil {
		openai.WriteError(w, errs.Internal(err))
		return
	}
	if s.cost != nil {
		if err := s.cost.RecordCapacity(r.Context(), report); err != nil {
			// The console state is already consistent; losing a cost sample is
			// not a reason to tell the operator their inventory failed, and it
			// shows up as a low coverage figure at close time.
			s.log.Warn("capacity sample not recorded", "cluster", report.Cluster.Name, "error", err)
		}
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
