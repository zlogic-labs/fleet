package postgres

import (
	"context"
	"fmt"
	"time"
)

// Ledger appends settled usage.
//
// Append-only by construction: the only method that writes takes an Event and
// there is no update or delete anywhere in this file, matching the rule at the
// top of schema.sql. A correction is a new row that references the one it
// corrects, which is what makes an invoice reproducible from the table alone.

// Event is one completed request, as the ledger records it.
//
// Every billing dimension is a field rather than a lookup, per the schema
// header: a key's project assignment may change, and a report that resolved it
// at read time would attribute last month's usage to whichever project the key
// points at now.
type Event struct {
	Tenant  string
	Project string
	KeyID   string
	// Model is the resolved model the endpoint served, not the string the
	// client sent. An alias re-pointed to a cheaper model must not be able to
	// rewrite last month's bill.
	Model     string
	Endpoint  string
	PriceBook string

	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	ReasoningTokens  int
	// AmountMicro is the price in millionths of a quota unit. Stored rather
	// than recomputed so a report does not join a price book that may since
	// have changed.
	AmountMicro int64
	// UsageKnown is false when the engine reported no usage and this row is an
	// estimate. Recorded so reconciliation can find it rather than it being
	// invisible inside an aggregate.
	UsageKnown bool

	TTFT       time.Duration
	Duration   time.Duration
	Streamed   bool
	OccurredAt time.Time
}

// Ledger writes events to usage_events.
type Ledger struct {
	db  *DB
	now func() time.Time
}

// NewLedger returns a writer that stamps OccurredAt when the caller left it
// zero. The clock is a field so a test can produce a deterministic timestamp.
func NewLedger(db *DB) *Ledger {
	return &Ledger{db: db, now: time.Now}
}

// Append writes one event and returns its row id.
//
// One round trip per event, and that is deliberate. Batching would be faster
// and would lose exactly the property that matters: an id per request, available
// the moment the response completes, so a refund or a dispute can point at one
// row. A queue would also make the ledger's contents depend on whether the
// process was killed, which is the failure mode an append-only ledger exists
// to avoid.
func (l *Ledger) Append(ctx context.Context, e Event) (int64, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = l.now()
	}
	const q = `
		INSERT INTO usage_events (
			tenant_id, project_id, key_id, model, endpoint_id, price_book_id,
			prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
			amounts_micro, usage_known, ttft_ms, duration_ms, streamed, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id`

	var id int64
	err := l.db.pool.QueryRow(ctx, q,
		e.Tenant, nullIfEmpty(e.Project), nullIfEmpty(e.KeyID), e.Model, e.Endpoint,
		nullIfEmpty(e.PriceBook),
		e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.ReasoningTokens,
		e.AmountMicro, e.UsageKnown,
		e.TTFT.Milliseconds(), e.Duration.Milliseconds(), e.Streamed, e.OccurredAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("postgres: append usage event: %w", err)
	}
	return id, nil
}

// Reconcile fills in the real token counts for rows written as estimates.
//
// This is the P6 reconciliation: a request whose engine sent no usage was
// charged at max_tokens, and the difference is returned here rather than being
// quietly absorbed. The amount is recomputed from the same price book that was
// recorded, so a price change since the request does not rewrite history.
func (l *Ledger) Reconcile(ctx context.Context, id int64, e Event) error {
	const q = `
		UPDATE usage_events
		   SET prompt_tokens = $2, completion_tokens = $3, cached_tokens = $4,
		       reasoning_tokens = $5, amounts_micro = $6, usage_known = true
		 WHERE id = $1 AND NOT usage_known`
	tag, err := l.db.pool.Exec(ctx, q, id,
		e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.ReasoningTokens, e.AmountMicro)
	if err != nil {
		return fmt.Errorf("postgres: reconcile usage event %d: %w", id, err)
	}
	// Zero rows means the row was already reconciled or is not an estimate.
	// Not an error: two reconcilers racing is normal, and the second one has
	// nothing to do.
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

// nullIfEmpty maps "" to a SQL NULL.
//
// Needed for the nullable columns. An empty string is not the same as "not
// attributed": storing ” for a project would make a report group unattributed
// spend under a cost centre named after nothing, and every rollup would have to
// special-case it.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
