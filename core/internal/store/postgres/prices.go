package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Price book storage.
//
// The gateway prices a request the moment it settles, so it needs prices in
// memory rather than a query per request. This file is the bridge: it reads the
// books out of Postgres and hands back a *billing.Pricer, and the refresh
// cadence is a policy decision rather than a fact of the schema.

// PriceStore reads price books and caches the ones in force.
type PriceStore struct {
	db *DB
	// pricer is the last successfully built pricer. Kept across a failed
	// refresh so that a brief database outage prices at yesterday's rate
	// rather than refusing every request.
	pricer *billing.Pricer
	// ids maps model to the id of the effective book, which the ledger records
	// so a row points at the exact prices that were applied.
	ids  map[string]string
	load time.Time
	// every is the reload interval. A price change should reach running
	// gateways without a restart, but not on every request.
	every time.Duration
}

// NewPriceStore returns a store that reloads at most once per every.
func NewPriceStore(db *DB, every time.Duration) *PriceStore {
	if every <= 0 {
		every = time.Minute
	}
	return &PriceStore{db: db, every: every, ids: map[string]string{}}
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
	const q = `
		SELECT id, model, input_rate, output_rate, cached_rate, reasoning_rate
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
		ids    = map[string]string{}
	)
	for rows.Next() {
		var (
			id     string
			price  billing.Price
			reason *int64
		)
		if err := rows.Scan(&id, &price.Model, &price.Input, &price.Output,
			&price.Cached, &reason); err != nil {
			return fmt.Errorf("postgres: scan price book: %w", err)
		}
		// NULL reasoning_rate means "same as output", matching
		// billing.Rate.Reasoning. Storing 0 would make the two disagree: the
		// arithmetic reads 0 as unset and the schema's CHECK as free.
		if reason != nil {
			price.Reasoning = *reason
		}
		prices = append(prices, price)
		ids[price.Model] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: iterate price books: %w", err)
	}

	book, err := billing.NewBook(prices)
	if err != nil {
		return fmt.Errorf("postgres: build price book: %w", err)
	}
	s.pricer = billing.NewPricer(book)
	s.ids = ids
	s.load = time.Now()
	return nil
}

// Pricer returns the pricer, reloading if it is stale.
//
// Staleness is checked here rather than by a background goroutine because a
// background refresh has no request to fail on: if it stopped, the gateway
// would keep pricing at yesterday's rate and nothing would say so.
func (s *PriceStore) Pricer(ctx context.Context) (*billing.Pricer, error) {
	if s.pricer == nil || time.Since(s.load) > s.every {
		if err := s.Refresh(ctx); err != nil && s.pricer == nil {
			return nil, err
		}
	}
	return s.pricer, nil
}

// Charge implements billing.Pricer.
//
// The refresh happens here rather than being left to the caller, because a
// caller that remembered to refresh and one that did not would price the same
// model two different ways, and only one of them would be in the ledger.
//
// A refresh that fails while a pricer already exists is ignored: stale prices
// beat no prices. See Pricer.
func (s *PriceStore) Charge(ctx context.Context, model string, u openai.Usage) (billing.Amount, error) {
	p, err := s.Pricer(ctx)
	if err != nil {
		return 0, err
	}
	return p.Charge(model, u)
}

// BookID returns the price book row id recorded on ledger events for a model.
//
// Empty when the model has no book, which the ledger stores as NULL. It exists
// so a ledger row points at the exact prices applied rather than at a model
// name whose price may since have changed.
func (s *PriceStore) BookID(model string) string { return s.ids[model] }

// PutPrice inserts a price book, closing the previous open-ended one.
//
// The close happens in the same transaction as the insert because the partial
// unique index would otherwise reject the second open book, and closing first
// without the insert would leave the model with no price at all if the insert
// failed.
func (s *PriceStore) PutPrice(ctx context.Context, p billing.Price, from time.Time) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	var reasoning *int64
	if p.Reasoning > 0 {
		v := p.Reasoning
		reasoning = &v
	}
	id := p.Model + "@" + from.UTC().Format("20060102T150405Z")

	err := s.db.inTx(ctx, func(tx pgx.Tx) error {
		// Close the open book at the moment the new one starts. Without an
		// explicit boundary the two overlap and a request in the overlap
		// matches either, which is why the schema forbids two open books.
		if _, err := tx.Exec(ctx,
			`UPDATE price_books SET effective_to = $2 WHERE model = $1 AND effective_to IS NULL`,
			p.Model, from); err != nil {
			return fmt.Errorf("postgres: close previous book: %w", err)
		}
		const ins = `
			INSERT INTO price_books
				(id, model, input_rate, output_rate, cached_rate, reasoning_rate, effective_from)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`
		if _, err := tx.Exec(ctx, ins, id, p.Model,
			p.Input, p.Output, p.Cached, reasoning, from); err != nil {
			return fmt.Errorf("postgres: insert price book: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
