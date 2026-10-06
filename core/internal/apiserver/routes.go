package apiserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zlogic-labs/fleet/core/pkg/httpx"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// The route table, and the constant the operator's contract has to agree with.
//
// Split from server.go the way the gateway splits the same two things: that file
// is what the control plane is made of and this one is what it answers.

const apiPrefix = "/api/v1"

// mustRouteTheContract fails at startup, not silently in production, if the
// route the control plane serves and the constant the operator PUTs to ever
// disagree. The two live in different files because chi splits the path, and
// a version bump that moved one and not the other would send every cluster's
// inventory into a void that answers 404.
func mustRouteTheContract() {
	if apiPrefix+"/inventory" != inventory.Path {
		panic("apiserver: the inventory route and inventory.Path disagree")
	}
}

func init() { mustRouteTheContract() }

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(cors(s.cfg.AllowedOrigins), httpx.RequestLog(s.log), httpx.Recoverer(s.log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	r.Get("/readyz", s.ready)

	r.Route(apiPrefix, func(r chi.Router) {
		// Everything under the API prefix carries money or credentials, so
		// everything under it is behind the admin token — including the
		// read-only routes. A registry listing is not sensitive on its own,
		// but an unauthenticated GET that returns 200 while the mutations
		// return 401 is a map of what is worth attacking.
		r.Use(requireAdmin(s.cfg.adminTokens()))

		// A wildcard, not a named parameter. A model name is owner/name, and
		// chi's {name} and {name:.+} both stop at the first slash, so
		// GET /models/Qwen/Qwen2.5 would 404 while
		// GET /models/Qwen%2FQwen2.5 would work. "*" takes the whole tail and
		// modelName unescapes it, so both forms land on the same entry.
		r.Get("/models", s.listModels)
		r.Get("/models/*", s.getModel)
		r.Delete("/models/*", s.deleteModel)
		// A repository is what is stored under a name: the weight set, whether
		// or not an engine can load it. Asking whether it is usable is a
		// property of that resource, so it is a GET returning the resource, not
		// a verb-named route that performs an action.
		//
		// It cannot live at /models/*/usability: chi will not match a fixed
		// segment after a wildcard, and /models/owner/usable would be
		// ambiguous with a model named "usable".
		r.Get("/repositories/*", s.getRepository)

		r.Post("/pulls", s.startPull)
		r.Get("/pulls", s.listPulls)
		r.Get("/pulls/{id}", s.getPull)
		r.Patch("/pulls/{id}", s.patchPull)

		r.Get("/storage", s.storageInfo)

		// The engine catalogue, so the console can show which engines can load
		// which model instead of offering a combination refused at admission.
		r.Get("/engines", s.listEngines)

		// The operator replaces what it observes. PUT rather than POST because
		// the payload is the whole cluster state and posting it twice must
		// leave the same state, not two copies.
		//
		// The path is split across this prefix and the leaf, while the operator
		// PUTs to the single constant inventory.Path. See mustRouteTheContract.
		r.Put("/inventory", s.reportInventory)
		r.Get("/clusters", s.clusterStatus)
		r.Get("/deployments", s.listDeployments)

		tenancyRoutes(r, s)

		// The cost pool. GPU-hour rates are what a GPU-hour costs the operator,
		// which Fleet cannot know. Token price books are keyed by model and
		// provider, because the same model costs three orders of magnitude more
		// from a vendor than from this fleet's own pool.
		r.Get("/cost-rates", s.listCostRates)
		r.Post("/cost-rates", s.putCostRate)
		r.Get("/price-books", s.listPriceBooks)
		r.Post("/price-books", s.putPriceBook)
		r.Get("/cost-periods", s.listCostPeriods)
		// PUT, not POST: closing creates the period resource at its own URL. A
		// second close recomputes rather than replacing, and once a later period
		// is closed the earlier one is frozen (11.10).
		r.Put("/cost-periods/{period}", s.closeCostPeriod)
		r.Get("/cost-periods/{period}", s.getCostPeriod)
		// The period still running. A separate collection rather than a query
		// on /cost-periods because it is not a period: there is no resource here
		// to close, get or delete.
		r.Get("/spend", s.getOpenSpend)

		// Fleet's own metering, audited against itself. Read-only, and not a
		// sub-resource of anything: it describes the ledger rather than one
		// tenant, one model or one period.
		r.Get("/usage-agreement", s.getUsageAgreement)
		// The other half of the same audit: whether the reporting copy holds
		// what the ledger holds. A separate route from the one above because
		// they answer different questions with different remedies, and because
		// one of them needs a store the other does not.
		r.Get("/usage-reconciliation", s.getUsageReconciliation)
	})

	return r
}
