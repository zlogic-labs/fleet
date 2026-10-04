package apiserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// tenancyRoutes registers the collections that hold money and credentials.
//
// They are top-level and plural, and an item's id may contain a slash, so item
// routes end in "*" rather than "{id}": chi's named parameters stop at the
// first slash, which would make acme/research unreachable while
// acme%2Fresearch worked.
//
// A separate function rather than a continuation of Handler, because the table
// had grown past the point where adding one route meant reading the next twenty
// lines to find where it went. The guard that matters -- that all of this sits
// behind requireAdmin -- stays in Handler, at the single place that establishes
// it, so a route added here cannot be quietly left unauthenticated by being
// added in the wrong block.
// ready reports whether this control plane can do its job. It fails on
// unreachable storage rather than on a missing dependency: a control plane that
// cannot reach its weights cannot start a pull, and a deployment it reports as
// ready will not be able to mount anything.
//
// It lives here rather than in Handler because it is the only handler with no
// dependency the operator configured, and grouping the two that answer "can
// you work" kept the route table below it readable.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	info := s.blobs.Info(r.Context())
	if !info.Reachable {
		openai.WriteError(w, errs.Unavailable("object storage is not reachable: %s", info.Message))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func tenancyRoutes(r chi.Router, s *Server) {
	r.Get("/tenants", s.listTenants)
	r.Post("/tenants", s.createTenant)
	r.Get("/tenants/*", s.getTenant)
	r.Patch("/tenants/*", s.updateTenant)
	r.Delete("/tenants/*", s.deleteTenant)

	r.Get("/projects", s.listProjects)
	r.Post("/projects", s.createProject)
	r.Get("/projects/*", s.getProject)
	r.Patch("/projects/*", s.updateProject)
	r.Delete("/projects/*", s.deleteProject)

	r.Get("/keys", s.listKeys)
	r.Post("/keys", s.createKey)
	r.Delete("/keys/*", s.deleteKey)

	r.Get("/budget-rules", s.listBudgetRules)
	r.Post("/budget-rules", s.putBudgetRule)
	r.Delete("/budget-rules/*", s.deleteBudgetRule)
}
