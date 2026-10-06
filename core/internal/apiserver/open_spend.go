package apiserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// The spend of the period that is still running.
//
// A closed period is an invoice and it is immutable. This is the other half:
// what has been charged so far this month, which is the only figure most
// operators look at daily and which until now existed only as a query nobody
// could run.
//
// It is deliberately not a cost report and must not become one. It carries no
// pool, no allocation and no coverage, because a pool is what capacity is sized
// against and this month's pool is not a measurement — it is whatever the fleet
// has happened to have so far. Printing it beside a real invoice would invite
// the comparison, and the comparison is wrong: it divides a month's spending by
// a few weeks of capacity.

type openSpendScope struct {
	// ID is "tenant/project", or bare "tenant" for unattributed spend. It is
	// the ledger's own scope string, so a row joins to the same key the
	// request was written under rather than to a second spelling of it.
	ID               string `json:"id"`
	Tenant           string `json:"tenant,omitempty"`
	Project          string `json:"project,omitempty"`
	UnitsMicro       int64  `json:"unitsMicro"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	CachedTokens     int64  `json:"cachedTokens"`
	// Estimated counts requests charged from a max_tokens guess because the
	// engine reported no usage (P6). Carried per row rather than as a total
	// because a total beside a number nobody trusts invites trusting it.
	Estimated int64 `json:"estimated"`
	// DirectMicro is the part of UnitsMicro a commercial provider charged.
	//
	// Carried per row rather than only in the totals because the interesting case
	// is a scope whose entire charge is direct: it has pool usage of zero and a
	// real bill, and a reader who saw only the pool number would conclude the
	// deployment ran on the operator's GPUs for free.
	DirectMicro int64 `json:"directMicro"`
}

type openSpendModel struct {
	Model             string `json:"model"`
	UnitsMicro        int64  `json:"unitsMicro"`
	Requests          int64  `json:"requests"`
	PromptTokens      int64  `json:"promptTokens"`
	CompletionTokens  int64  `json:"completionTokens"`
	CachedTokens      int64  `json:"cachedTokens"`
	EstimatedRequests int64  `json:"estimatedRequests"`
}

type openSpendResponse struct {
	Period string `json:"period"`
	// From and To bound the figures. To is the end of the calendar month even
	// though nothing has happened past now, so the response names the period
	// these numbers belong to rather than leaving the caller to infer it.
	From string `json:"from"`
	To   string `json:"to"`
	// AsOf is when the figures were read. A running period changes underneath
	// the reader, and a report that cannot say how fresh it is is a number
	// someone will quote in an argument.
	AsOf   string           `json:"asOf"`
	Scopes []openSpendScope `json:"scopes"`
	Models []openSpendModel `json:"models"`
	// Totals and the two counts below are over every scope, so a caller that
	// renders a filtered table can still show the whole without summing.
	TotalsMicro int64 `json:"totalsMicro"`
	Requests    int64 `json:"requests"`
	Estimated   int64 `json:"estimated"`
	// Priced is false when no GPU-hour rate is declared. The figures are token
	// charges and exist either way, but an operator reading a total next to an
	// unpriced pool would reasonably take it for the whole bill.
	Priced bool `json:"priced"`
	// DirectMicro and PoolMicro split TotalsMicro into the two cost centres.
	//
	// Two numbers rather than one "total" because they are not the same kind of
	// thing and must not be compared: PoolMicro is a weighting that becomes money
	// only when the pool is closed and divided, DirectMicro is money the vendors
	// already charged. Adding them is what the split exists to prevent, so the
	// console adds nothing and the comment here says so out loud.
	PoolMicro   int64 `json:"poolMicro"`
	DirectMicro int64 `json:"directMicro"`
	// Providers is the vendor breakdown, for reconciling against the invoices.
	Providers []openSpendProvider `json:"providers"`
}

// openSpendProvider is one vendor's charges in the running period.
type openSpendProvider struct {
	Provider string `json:"provider"`
	// Amount is real money.
	Amount int64 `json:"amount"`
	// Requests lets a mismatch be narrowed to volume versus unit price.
	Requests int64 `json:"requests"`
}

// getOpenSpend reports what has been charged in the period still running.
//
// Read-only, and it stays read-only: closing a period is a separate PUT
// because it is the event that makes the figures immutable. One endpoint that
// both reports and closes would let a GET change an invoice.
func (s *Server) getOpenSpend(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	period := cost.NewPeriod(now)

	// Bounded by the period's end rather than by now, so this is the same
	// window the eventual close will use. Anything else would make the open
	// figure and the invoice disagree for reasons that have nothing to do with
	// the data.
	window := postgres.Window{From: period.Start, To: period.End}

	scopes, err := s.db.SpendByScope(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}
	models, err := s.db.SpendByModel(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}
	providers, err := s.db.SpendByProvider(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}
	rates, err := s.cost.ListRates(ctx)
	if err != nil {
		failInternal(w, err)
		return
	}

	out := openSpendResponse{
		Period:    period.String(),
		From:      period.Start.Format(time.RFC3339),
		To:        period.End.Format(time.RFC3339),
		AsOf:      now.Format(time.RFC3339),
		Scopes:    make([]openSpendScope, 0, len(scopes)),
		Models:    make([]openSpendModel, 0, len(models)),
		Providers: make([]openSpendProvider, 0, len(providers)),
		Priced:    len(rates) > 0,
	}
	for _, s := range scopes {
		row := openSpendScope{
			ID: s.Key, UnitsMicro: s.UnitsMicro, Requests: s.Requests,
			PromptTokens: s.PromptTokens, CompletionTokens: s.CompletionTokens,
			CachedTokens: s.CachedTokens, Estimated: s.Estimated,
			DirectMicro: s.DirectMicro,
		}
		if tenant, project, ok := strings.Cut(s.Key, "/"); ok {
			row.Tenant, row.Project = tenant, project
		} else {
			row.Tenant = s.Key
		}
		out.Scopes = append(out.Scopes, row)
		out.TotalsMicro += s.UnitsMicro
		out.Requests += s.Requests
		out.Estimated += s.Estimated
		out.DirectMicro += s.DirectMicro
		out.PoolMicro += s.UnitsMicro - s.DirectMicro
	}
	for _, m := range models {
		out.Models = append(out.Models, openSpendModel{
			Model: m.Key, UnitsMicro: m.UnitsMicro, Requests: m.Requests,
			PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
			CachedTokens: m.CachedTokens, EstimatedRequests: m.Estimated,
		})
	}
	for _, p := range providers {
		out.Providers = append(out.Providers, openSpendProvider{
			Provider: p.Provider, Amount: p.Amount, Requests: p.Requests,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
