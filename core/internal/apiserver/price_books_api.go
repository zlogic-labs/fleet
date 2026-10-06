package apiserver

import (
	"net/http"
	"strings"
	"time"

	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Token price books.
//
// A price book is what a request is charged, and it is keyed by a model and a
// provider rather than by a model alone. That distinction is the whole reason
// this resource exists: the same weights served from this fleet's own pool are
// a share of a fixed cost, while the identical weights from a vendor are billed
// at the vendor's list price — three orders of magnitude apart. One index on
// model alone would let whichever book loaded last price both.
//
// An empty provider means this fleet's own capacity, which is the default
// because a deployment with no vendor configured is the ordinary case.

// priceView is the wire shape of a book.
//
// Its own type rather than billing.Price, because two of these fields are not
// part of a price: the row id exists so an operator chasing a disputed charge
// can be handed the exact book, and the effective date is a fact about when it
// took force. Neither belongs on the arithmetic type that both the gateway and
// the pricer hold.
type priceView struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	billing.Rate
	// EffectiveFrom is when this book started applying. Not "now": a vendor's
	// announced price rise is declared before it takes effect.
	EffectiveFrom time.Time `json:"effectiveFrom"`
}

// priceRequest is what an operator declares. Only provider is optional.
type priceRequest struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	billing.Rate
	// EffectiveFrom defaults to now. Accepted in the past too — backdating is
	// how an operator corrects a month that has not been closed.
	EffectiveFrom *time.Time `json:"effectiveFrom,omitempty"`
}

// listPriceBooks returns the books currently in force.
func (s *Server) listPriceBooks(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	rows, err := s.prices.ListBooks(r.Context())
	if err != nil {
		failInternal(w, err)
		return
	}
	out := make([]priceView, 0, len(rows))
	for _, b := range rows {
		out = append(out, priceViewOf(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func priceViewOf(b sqlstore.Book) priceView {
	return priceView{
		ID: b.ID, Model: b.Model, Provider: billing.NormalizeProvider(b.Provider),
		Rate:          b.Rate,
		EffectiveFrom: b.EffectiveFrom,
	}
}

// putPriceBook declares a book's rates, closing the previous open one.
//
// The close happens in the same transaction as the insert, so a request can
// never match two books and a failed insert cannot leave a model unpriced.
func (s *Server) putPriceBook(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	var req priceRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Provider) != "" && billing.NormalizeProvider(req.Provider) == "" {
		// Normalising to empty here would silently redeclare the fleet's own
		// book, and a blank-looking vendor name would end up pricing requests
		// against a GPU-hour weight.
		writeError(w, errs.InvalidArgument("provider: must be a name, or empty for this fleet's own capacity"))
		return
	}
	price := billing.Price{
		Model:    strings.TrimSpace(req.Model),
		Provider: req.Provider,
		Rate:     req.Rate,
	}
	if err := price.Validate(); err != nil {
		writeError(w, errs.InvalidArgument("%s", cleanErr(err)))
		return
	}
	from := time.Now()
	if req.EffectiveFrom != nil {
		from = req.EffectiveFrom.UTC()
	}
	id, err := s.prices.PutPrice(r.Context(), price, from)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, priceView{
		ID: id, Model: price.Model, Provider: billing.NormalizeProvider(price.Provider),
		Rate: price.Rate, EffectiveFrom: from,
	})
}
