package apiserver

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// The cost pool (P8).
//
// Closing is POST on a named collection rather than PUT on an item, because
// what it creates is the close event: two operators clicking at once must
// produce one invoice and one 409, not two invoices or a silent overwrite.

// listCostRates returns the declared GPU-hour prices.
func (s *Server) listCostRates(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	rates, err := s.cost.ListRates(r.Context())
	if err != nil {
		failInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rates)
}

// putCostRate declares what a GPU-hour costs in a cluster.
func (s *Server) putCostRate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var rate cost.Rate
	if !decode(w, r, &rate) {
		return
	}
	if err := s.cost.PutRate(r.Context(), rate); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rate)
}

// listCostPeriods returns closed periods, newest first.
func (s *Server) listCostPeriods(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	periods, err := s.cost.ListPeriods(r.Context())
	if err != nil {
		failInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, periods)
}

// getCostPeriod returns one closed period with its allocations.
func (s *Server) getCostPeriod(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	period := periodParam(w, r, "period")
	if !period.Valid() {
		return
	}
	rep, err := s.cost.GetPeriod(r.Context(), period.String())
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// closeCostPeriod computes and stores a period's cost report.
func (s *Server) closeCostPeriod(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	period := periodParam(w, r, "period")
	if !period.Valid() {
		return
	}
	rep, err := s.cost.ClosePeriod(r.Context(), period, coverageFor(r))
	if err != nil {
		var incomplete *cost.ErrIncomplete
		if errors.As(err, &incomplete) {
			// 400 and not 500: the request was fine, the observations are not
			// there yet. The operator's reports will catch up and the same call
			// will then succeed, which is what a retry loop needs to be told.
			writeError(w, errs.InvalidArgument("%s", cleanErr(err)))
			return
		}
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rep)
}

func periodParam(w http.ResponseWriter, r *http.Request, name string) cost.Period {
	period, err := cost.ParsePeriod(chi.URLParam(r, name))
	if err != nil {
		writeError(w, errs.InvalidArgument("%s", err))
		return cost.Period{}
	}
	return period
}

// coverageFor reads an optional minimum-coverage fraction.
//
// It cannot be raised above the default silently by accident, because the
// caller has to say so out loud in the query string, and the report records the
// coverage it was actually computed from either way.
func coverageFor(r *http.Request) float64 {
	raw := r.URL.Query().Get("minCoverage")
	if raw == "" {
		return cost.DefaultMinCoverage
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || f > 1 {
		return cost.DefaultMinCoverage
	}
	return f
}
