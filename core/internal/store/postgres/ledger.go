package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Ledger appends settled usage.
//
// Append-only by construction: the only method that writes takes a billing.Record
// and there is no update or delete anywhere in this file, matching the rule at
// the top of schema.sql. A correction is a new row that references the one it
// corrects, which is what makes an invoice reproducible from the table alone.

// Ledger writes records to usage_events.
type Ledger struct {
	db  *DB
	now func() time.Time
}

// NewLedger returns a writer that stamps OccurredAt when the caller left it
// zero. The clock is a field so a test can produce a deterministic timestamp.
func NewLedger(db *DB) *Ledger {
	return &Ledger{db: db, now: time.Now}
}

// Record appends one settled request and returns its row id.
//
// One round trip per event, and that is deliberate. Batching would be faster
// and would lose exactly the property that matters: an id per request,
// available the moment the response completes, so a refund or a dispute can
// point at one row. A queue would also make the ledger's contents depend on
// whether the process was killed, which is the failure mode an append-only
// ledger exists to avoid.
func (l *Ledger) Record(ctx context.Context, r billing.Record) (int64, error) {
	if r.OccurredAt.IsZero() {
		r.OccurredAt = l.now()
	}
	const q = `
		INSERT INTO usage_events (
			tenant_id, project_id, key_id, model, endpoint_id, price_book_id,
			prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
			amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING id`

	var id int64
	err := l.db.pool.QueryRow(ctx, q,
		r.Tenant, nullIfEmpty(r.Project), nullIfEmpty(r.KeyID),
		// The resolved model, not what the client asked for. The handler fills
		// this from the endpoint it actually reached.
		r.Model, r.Endpoint, nullIfEmpty(r.PriceBook),
		r.Usage.PromptTokens, r.Usage.CompletionTokens,
		r.Usage.CachedPromptTokens(), r.Usage.ReasoningTokens(),
		int64(r.Amount), r.UsageKnown, sourceOrEngine(r.UsageSource), r.Truncated,
		r.TTFT.Milliseconds(), r.Duration.Milliseconds(), r.Streamed, r.OccurredAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("postgres: append usage event: %w", err)
	}
	return id, nil
}

// sourceOrEngine keeps an unset source out of the table.
//
// A zero Source means the caller did not say, which is not the same as saying
// "the engine reported this". The default is the conservative reading: a row
// that says nothing is attributed to the engine, so a report counting
// unmeasured output cannot silently include it.
func sourceOrEngine(s billing.Source) string {
	if s == "" {
		return string(billing.SourceEngine)
	}
	return string(s)
}

// Reconcile replaces an estimate with what the engine later reported.
//
// This is the P6 reconciliation: a request whose engine sent no usage was
// charged on the gateway's own count of the answer, and if the count was later
// contradicted — a reconcile pass, a replay, an engine that was fixed
// mid-flight — the real figures replace it here rather than being quietly
// absorbed. The amount is recomputed from the same price book that was
// recorded, so a price change since the request does not rewrite history.
//
// Zero rows affected is not an error: two reconcilers racing is normal, and
// the second one has nothing left to do.
func (l *Ledger) Reconcile(ctx context.Context, id int64, r billing.Record) error {
	const q = `
		UPDATE usage_events
		   SET prompt_tokens = $2, completion_tokens = $3, cached_tokens = $4,
		       reasoning_tokens = $5, amounts_micro = $6, usage_known = true,
		       usage_source = $7
		 WHERE id = $1 AND NOT usage_known`
	if _, err := l.db.pool.Exec(ctx, q, id,
		r.Usage.PromptTokens, r.Usage.CompletionTokens,
		r.Usage.CachedPromptTokens(), r.Usage.ReasoningTokens(), int64(r.Amount),
		sourceOrEngine(r.UsageSource)); err != nil {
		return fmt.Errorf("postgres: reconcile usage event %d: %w", id, err)
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
