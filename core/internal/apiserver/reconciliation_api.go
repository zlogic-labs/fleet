package apiserver

import (
	"context"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Does the reporting copy agree with the books.
//
// The metering audit next to this one asks whether the gateway measured a
// request the way the engine did. This asks a different question with a
// different remedy: whether the replica a report is read from holds the rows the
// ledger holds. A replica that dropped a batch under load, or was restored from
// an older backup, or had a row written twice by the backfill catching up behind
// the queue, produces two stores that both look healthy and disagree — and every
// report reads from one of them and therefore agrees with itself.
//
// Read-only, and it has to be. The ledger is authoritative, so a difference is a
// fact about the replica, not a correction to the books: recomputing anything
// from it would move figures a closed period was already invoiced from, which
// §11.10 exists to prevent.

// detailLag is how far short of now the comparison stops when the window is the
// month still running.
//
// The mirror is written through a queue that flushes every few seconds and drops
// rather than blocks. A comparison that ran to this instant would report the
// last few seconds as missing rows every single time — true and useless, the
// same way a rate limit is not judged on the request in flight. Two minutes is
// far more than the flush interval, and it is not a budget for anything else:
// the rows between the window's end and now are simply not compared by this
// report, and the next one includes them.
const detailLag = 2 * time.Minute

// reconciliationResponse is the whole report.
type reconciliationResponse struct {
	Period string `json:"period"`
	From   string `json:"from"`
	To     string `json:"to"`
	AsOf   string `json:"asOf"`
	// LagSeconds is how far the window stops short of now. Zero for a period
	// that has ended; non-zero means the comparison is deliberately incomplete,
	// which a reader has to know before treating a missing row as a fault.
	LagSeconds int `json:"lagSeconds"`
	// Available is false when there is nothing to compare against. That is a
	// supported deployment rather than an error, and Note says which of the two
	// reasons it is: nothing configured, or something configured that did not
	// answer.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	// GroupLimit is the policy in force, echoed for the same reason the metering
	// audit echoes its tolerance: the same rows under a different cap are a
	// different report, and a finding nobody can reproduce is a rumour.
	GroupLimit int `json:"groupLimit"`
	billing.Reconciliation
}

// getUsageReconciliation compares the ledger with the replica.
func (s *Server) getUsageReconciliation(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	period, window, lag, err := s.reconciliationWindow(ctx, r, now)
	if err != nil {
		writeError(w, err)
		return
	}

	ledger, err := s.db.UsageTally(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}
	ledgerGroups, err := s.db.UsageByGroup(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}

	out := reconciliationResponse{
		Period:     period.String(),
		From:       window.From.Format(time.RFC3339),
		To:         window.To.Format(time.RFC3339),
		AsOf:       now.Format(time.RFC3339),
		LagSeconds: lag,
		GroupLimit: billing.DefaultGroupLimit,
	}

	replica, reason := s.detail.get()
	if replica == nil {
		// What the ledger holds is still worth reporting without one: it is the
		// side that cannot be regenerated, and it is the number an operator
		// would otherwise have to read with SQL.
		out.Note = reason
		out.Ledger = ledger
		writeJSON(w, http.StatusOK, out)
		return
	}

	replicaTally, err := replica.DetailTally(ctx, window.From, window.To)
	if err != nil {
		failInternal(w, err)
		return
	}
	replicaGroups, err := replica.DetailGroups(ctx, window.From, window.To)
	if err != nil {
		failInternal(w, err)
		return
	}

	out.Available = true
	out.Reconciliation = billing.Reconcile(
		ledger, replicaTally, ledgerGroups, replicaGroups, billing.DefaultGroupLimit)
	writeJSON(w, http.StatusOK, out)
}

// reconciliationWindow turns the request into the range being compared.
func (s *Server) reconciliationWindow(ctx context.Context, r *http.Request, now time.Time) (cost.Period, postgres.Window, int, error) {
	period, err := s.comparisonPeriod(ctx, r, now)
	if err != nil {
		return cost.Period{}, postgres.Window{}, 0, err
	}

	w := postgres.Window{From: period.Start, To: period.End}
	lag := 0
	if w.To.After(now) {
		w.To = now.Add(-detailLag)
		lag = int(detailLag.Seconds())
	}
	if !w.To.After(w.From) {
		return cost.Period{}, postgres.Window{}, 0, errs.InvalidArgument(
			"%s began less than %s ago, and the mirror is given that long to catch up before it is compared",
			period, detailLag)
	}
	return period, w, lag, nil
}

// comparisonPeriod picks the window when the request does not name one.
//
// The most recently closed period, because that is the one whose figures
// somebody has already been shown, and "can I reproduce this invoice" is the
// question worth answering first. With nothing closed it falls back to the
// previous calendar month rather than the one running: a month that has ended is
// compared in full, so the answer does not change while the operator looks at
// it.
func (s *Server) comparisonPeriod(ctx context.Context, r *http.Request, now time.Time) (cost.Period, error) {
	if raw := r.URL.Query().Get("period"); raw != "" {
		period, err := cost.ParsePeriod(raw)
		if err != nil {
			// A period the caller typed wrong is the caller's mistake, not a
			// fault: without this the raw parse error has no kind and the
			// console is told "internal error" for a typo.
			return cost.Period{}, errs.InvalidArgument("%s", err)
		}
		return period, nil
	}
	periods, err := s.cost.ListPeriods(ctx)
	if err != nil {
		return cost.Period{}, err
	}
	for _, p := range periods {
		// A period that cannot be parsed is skipped rather than failing the
		// report: it is a row in a table this process does not own, and the
		// answer an operator wants does not depend on it.
		if parsed, err := cost.ParsePeriod(p.Period); err == nil {
			return parsed, nil
		}
	}
	return cost.NewPeriod(now).Previous(), nil
}
