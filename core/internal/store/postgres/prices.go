package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Price book storage.
//
// The gateway prices a request the moment it settles, so it needs prices in
// memory rather than a query per request. This file is the bridge: it reads the
// books out of Postgres and hands back a *billing.Pricer, and the refresh
// cadence is a policy decision rather than a fact of the schema.

// prices is one consistent view of the books in force.
//
// The three values belong together and are swapped together. They were three
// separate fields, written by Refresh and read by Pricer and BookID on every
// settled request, with no lock anywhere: two concurrent settlements both saw
// a stale timestamp and both replaced the fields, and a reader could see a new
// pricer with the previous map. That map is the worse half — reading a Go map
// while another goroutine writes it is a panic, not a stale value, and the
// gateway panics on a price refresh under load.
//
// bookKey identifies one price book: a model and who served it.
//
// A struct rather than a joined string so a model called "gpt-4o" from a provider
// called "4o" cannot collide with a model called "gpt" from "4o@..." — the
// separator choice would be a decision this package would then have to be right
// about forever, for no benefit.
type bookKey struct {
	model    string
	provider billing.Provider
}

type priceSnapshot struct {
	pricer *billing.Pricer
	// ids maps a model and provider to the id of the effective book, which the
	// ledger records so a row points at the exact prices that were applied.
	ids  map[bookKey]string
	load time.Time
}

// PriceStore reads price books and caches the ones in force.
type PriceStore struct {
	db *DB
	// mu guards held. It is never held across the query in Refresh: a reload
	// is I/O, and blocking every settlement on it would turn a slow price
	// lookup into a slow gateway.
	mu    sync.RWMutex
	held  priceSnapshot
	every time.Duration
}

// NewPriceStore returns a store that reloads at most once per every.
func NewPriceStore(db *DB, every time.Duration) *PriceStore {
	if every <= 0 {
		every = time.Minute
	}
	return &PriceStore{
		db:    db,
		every: every,
		held:  priceSnapshot{ids: map[bookKey]string{}},
	}
}

// Refresh rebuilds the pricer from every model's currently effective book.
//
// "Currently effective" is decided by the query, not here: a book applies when
// effective_from <= now and (effective_to IS NULL OR effective_to > now). The
// partial unique index price_books_one_open_per_model guarantees at most one
// open-ended book per model, so this cannot be ambiguous.
//
// An invalid row fails the whole refresh rather than being skipped. A price
// that fails billing.Price.Validate is an operator error, and quietly dropping
// it would price that model at nothing — the one failure mode this package
// exists to make impossible. The previous pricer survives, so the failure is
// loud and the gateway keeps working.
func (s *PriceStore) Refresh(ctx context.Context) error {
	// A missing pool is reported, not dereferenced. pgxpool's methods panic
	// on a nil receiver rather than returning an error, so this is the one
	// path through the store that crashes the process instead of failing a
	// request -- and it is reached exactly when configuration is incomplete,
	// which is when a confusing stack trace helps least.
	if s.db == nil || s.db.pool == nil {
		return errors.New("postgres: read price books: no database connection pool")
	}

	const q = `
		SELECT id, model, provider, input_rate, output_rate, cached_rate, reasoning_rate
		  FROM price_books
		 WHERE effective_from <= now()
		   AND (effective_to IS NULL OR effective_to > now())`

	rows, err := s.db.pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("postgres: read price books: %w", err)
	}
	defer rows.Close()

	var (
		prices []billing.Price
		ids    = map[bookKey]string{}
	)
	for rows.Next() {
		var (
			id     string
			price  billing.Price
			reason *int64
		)
		if err := rows.Scan(&id, &price.Model, &price.Provider, &price.Input, &price.Output,
			&price.Cached, &reason); err != nil {
			return fmt.Errorf("postgres: scan price book: %w", err)
		}
		// NULL reasoning_rate means "same as output", matching
		// billing.Rate.Reasoning. Storing 0 would make the two disagree: the
		// arithmetic reads 0 as unset and the schema's CHECK as free.
		if reason != nil {
			price.Reasoning = *reason
		}
		price.Provider = billing.NormalizeProvider(price.Provider)
		prices = append(prices, price)
		ids[bookKey{price.Model, price.Provider}] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: iterate price books: %w", err)
	}

	book, err := billing.NewBook(prices)
	if err != nil {
		return fmt.Errorf("postgres: build price book: %w", err)
	}
	// Swapped in one assignment under a short lock. The query above ran
	// unlocked on purpose: holding a write lock across a database round trip
	// would serialise every settlement behind it.
	s.mu.Lock()
	s.held = priceSnapshot{pricer: billing.NewPricer(book), ids: ids, load: time.Now()}
	s.mu.Unlock()
	return nil
}

// Pricer returns the pricer, reloading if it is stale.
//
// Staleness is checked here rather than by a background goroutine because a
// background refresh has no request to fail on: if it stopped, the gateway
// would keep pricing at yesterday's rate and nothing would say so.
func (s *PriceStore) Pricer(ctx context.Context) (*billing.Pricer, error) {
	if s.stale() {
		if err := s.Refresh(ctx); err != nil {
			// Fatal only while there is nothing to fall back on.
			s.mu.RLock()
			first := s.held.pricer == nil
			s.mu.RUnlock()
			if first {
				return nil, err
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.held.pricer, nil
}

// stale reports whether the held books should be reloaded.
func (s *PriceStore) stale() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.held.pricer == nil || time.Since(s.held.load) > s.every
}

// Charge implements billing.Pricer.
//
// The refresh happens here rather than being left to the caller, because a
// caller that remembered to refresh and one that did not would price the same
// model two different ways, and only one of them would be in the ledger.
//
// A refresh that fails while a pricer already exists is ignored: stale prices
// beat no prices. See Pricer.
func (s *PriceStore) Charge(ctx context.Context, model string, provider billing.Provider, u openai.Usage) (billing.Amount, error) {
	p, err := s.Pricer(ctx)
	if err != nil {
		return 0, err
	}
	return p.Charge(model, provider, u)
}

// BookID returns the price book row id recorded on ledger events for a model
// from a provider.
//
// Empty when the pair has no book, which the ledger stores as NULL. It exists
// so a ledger row points at the exact prices applied rather than at a model
// name whose price may since have changed — and, now that one model can be
// priced by several sources, at a model name that no longer says which.
func (s *PriceStore) BookID(model string, provider billing.Provider) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.held.ids[bookKey{model, billing.NormalizeProvider(provider)}]
}

// Predict implements billing.PricerSource.
//
// It reloads first for the same reason Charge does: a budget's reservation has
// to be against the current book, and a book that failed to load must read as
// "nothing priced" so the reservation falls through to a zero rather than to a
// stale rate.
func (s *PriceStore) Predict(ctx context.Context, model string, provider billing.Provider, promptTokens, maxTokens int) (billing.Prediction, error) {
	p, err := s.Pricer(ctx)
	if err != nil {
		return billing.Prediction{}, err
	}
	return p.Predict(model, provider, promptTokens, maxTokens), nil
}
