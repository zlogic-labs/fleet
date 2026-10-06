package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Price books: declaring and reading them.
//
// Separate from prices.go because that file's job is the gateway's hot path —
// it answers "what do I charge for this request" thousands of times a minute,
// and it can only do that because everything it reads comes from one cached
// snapshot. Declaring and listing books are operator actions that happen about
// once a month, and they have no business sharing a lock with a settlement.

// Book is one price book row, as stored.
//
// A type of its own rather than billing.Price because two of these fields are
// not part of a price's identity: the row id exists so an operator chasing a
// disputed charge can be handed the exact book, and the effective date is a
// fact about when it took force. billing.Price carries neither, and adding
// either would put a database concern on the type the pricer holds per request.
//
// No currency, though billing.Price has one: a rate here is quota units per
// token, not money, so a currency would be a label that never reaches a balance
// and would differ between two books whose rates are identical. The money
// figure is a closed period's allocation, and the currency that goes with it is
// declared per cluster on the GPU-hour rate.
type Book struct {
	ID       string
	Model    string
	Provider billing.Provider
	billing.Rate
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
}

// ListBooks returns the books currently in force.
//
// Only the effective ones. A closed book is history, and listing it beside its
// replacement would show an operator two prices for one model with nothing to
// say which one a given request paid.
func (s *PriceStore) ListBooks(ctx context.Context) ([]Book, error) {
	const q = `
		SELECT id, model, provider, input_rate, output_rate, cached_rate, reasoning_rate,
		       effective_from
		  FROM price_books
		 WHERE effective_from <= now()
		   AND (effective_to IS NULL OR effective_to > now())
		 ORDER BY model, provider`
	rows, err := s.db.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: list price books: %w", err)
	}
	defer rows.Close()

	out := []Book{}
	for rows.Next() {
		var (
			b      Book
			reason *int64
		)
		if err := rows.Scan(&b.ID, &b.Model, &b.Provider, &b.Input, &b.Output, &b.Cached,
			&reason, &b.EffectiveFrom); err != nil {
			return nil, fmt.Errorf("postgres: scan price book: %w", err)
		}
		if reason != nil {
			b.Reasoning = *reason
		}
		b.Provider = billing.NormalizeProvider(b.Provider)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate price books: %w", err)
	}
	return out, nil
}

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
	// Normalised before it reaches either the id or the row, so the same vendor
	// cannot hold two open books that differ only in capitalisation.
	provider := billing.NormalizeProvider(p.Provider)
	// Truncated to the resolution the id is built at, for two reasons that are
	// really one. A finer instant than the id cannot distinguish is a value
	// nothing can ever refer to, and it puts the reader on the wrong side of a
	// boundary: the row is only visible once the reader's own clock has passed
	// it, and that clock belongs to another machine. Between a control plane and
	// a database the two disagree by tens or hundreds of milliseconds and drift,
	// so a book declared "now" can read back as not yet in force. Truncating
	// gives the whole of the declaring second as slack instead of a knife edge
	// at a random offset within it.
	from = from.UTC().Truncate(time.Second)
	id := bookID(p.Model, provider, from)

	err := s.db.inTx(ctx, func(tx pgx.Tx) error {
		// Where the open book for this model and provider begins, if there is
		// one. Read rather than assumed, because the three cases below need it.
		//
		// Scoped to the provider as well as the model throughout: closing the
		// fleet's book because a vendor's was repriced would leave the fleet's
		// models with no price at all, and every one of them would fall back to
		// the floor rate.
		//
		// Locked, not merely read. Without it another declaration can close
		// this row between the read and the write below, and the same-second
		// branch would then delete a closed book — a period that has already
		// been charged against, which is the one thing §11.10 exists to
		// prevent. With the row held, the guard on the DELETE below is a fact
		// rather than a hope, and two declarations cannot interleave at all.
		var openAt *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT effective_from FROM price_books
			  WHERE model = $1 AND provider = $2 AND effective_to IS NULL
			  FOR UPDATE`,
			p.Model, provider).Scan(&openAt); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: read the open book: %w", err)
		}

		switch {
		case openAt == nil:
			// Nothing to supersede.

		case bookSecond(*openAt) == bookSecond(from):
			// The id is derived from the effective date to the second, so
			// declaring the same model twice inside one second asks for a row
			// that already exists. That is not a corner case: a retry, or
			// backfilling one effective date with two revisions, does exactly
			// this, and the caller would get an error for an operator action
			// that means something.
			//
			// Compared to the second rather than to the instant, because that
			// is the resolution the id is built at: two calls a few hundred
			// milliseconds apart share an id while their instants differ, and
			// treating those as different books produces an interval that ends
			// before it begins, which the schema rejects.
			//
			// Replaced rather than closed, for the same reason. Nothing is lost
			// by removing it: the whole of its life falls inside this second,
			// which is the instant the replacement takes force.
			if _, err := tx.Exec(ctx,
				`DELETE FROM price_books WHERE id = $1 AND effective_to IS NULL`, id); err != nil {
				return fmt.Errorf("postgres: replace same-second book: %w", err)
			}

		case openAt.After(from):
			// The new book starts before the open one, so closing the open one
			// at this instant would end it before it began.
			//
			// Refused rather than accommodated, because the period asked for is
			// already covered: a book has been in force over it since the open
			// one started. Changing it means rewriting a period that has already
			// been priced, which is what section 11.10 exists to prevent, so it
			// should be a deliberate act rather than a side effect of declaring
			// a new rate.
			return fmt.Errorf("%w: %s@%s is already priced from %s, which is later than %s; declaring a book from %s would rewrite a priced period",
				conflict, p.Model, providerLabel(provider),
				openAt.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339), from.UTC().Format("2006-01-02"))

		default:
			// Close the open book at the moment the new one starts. Without an
			// explicit boundary the two overlap and a request in the overlap
			// matches either, which is why the schema forbids two open books.
			//
			// Scoped to the provider, not just the model: the fleet's book and
			// a vendor's book for the same model are two prices that move
			// independently, and closing on the model alone ends both the
			// moment a vendor is repriced. Nothing reports that — the fleet's
			// requests simply start being charged at the floor rate.
			if _, err := tx.Exec(ctx,
				`UPDATE price_books SET effective_to = $3
				  WHERE model = $1 AND provider = $2 AND effective_to IS NULL`,
				p.Model, provider, from); err != nil {
				return fmt.Errorf("postgres: close previous book: %w", err)
			}
		}

		const ins = `
			INSERT INTO price_books
				(id, model, provider, input_rate, output_rate, cached_rate, reasoning_rate, effective_from)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
		if _, err := tx.Exec(ctx, ins, id, p.Model, provider,
			p.Input, p.Output, p.Cached, reasoning, from); err != nil {
			// Classified, because this is reachable: backdating onto a second
			// that holds a closed book leaves the delete above untouched, and an
			// operator deserves a 409 for that rather than a 500.
			return wrap(err, "insert price book")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// providerLabel names a provider for a message, standing in for "the fleet"
// where there is none.
func providerLabel(p billing.Provider) string {
	if p == "" {
		return "the fleet's own capacity"
	}
	return string(p)
}

// bookID names a price book row.
//
// The provider is in the middle rather than at the end because a book's id is
// quoted back in the ledger and read by an operator chasing a disputed charge:
// "gpt-4o@openai@20260901…" reads as what it is, where "gpt-4o@20260901…" does
// not say which rate it was.
func bookID(model, provider string, from time.Time) string {
	stamp := bookSecond(from)
	if provider == "" {
		return model + "@" + stamp
	}
	return model + "@" + provider + "@" + stamp
}

// bookSecond is the resolution a book's identity is built at.
//
// Exists so that "do these two declarations collide" and "what will this id
// be" cannot be answered differently: a caller that compared instants where
// the id compares seconds would find no collision right up until the insert
// failed.
func bookSecond(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}
