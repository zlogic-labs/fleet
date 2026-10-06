package postgres

import (
	"context"
	"fmt"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The ledger's side of the reconciliation.
//
// Two queries, because the totals and the groups are not the same measurement.
// A GROUP BY cannot report rows whose grouping key is empty, so a replica that
// lost exactly the unattributed rows would reconcile clean against a ledger that
// still holds them.
//
// Every SUM is COALESCEd and cast. COALESCEd because the token breakdown is
// nullable — an engine that reports totals without one has said nothing, and SUM
// over a group where every row said nothing is NULL rather than zero, which
// fails the scan on a month that is perfectly healthy. Cast because SUM over a
// bigint is numeric in PostgreSQL, and a numeric that does not fit an int64
// fails the scan rather than converting.

const ledgerTallyColumns = `COUNT(*),
	       COALESCE(SUM(prompt_tokens), 0)::bigint,
	       COALESCE(SUM(completion_tokens), 0)::bigint,
	       COALESCE(SUM(cached_tokens), 0)::bigint,
	       COALESCE(SUM(amounts_micro), 0)::bigint`

// UsageTally totals the ledger over a half-open window.
func (db *DB) UsageTally(ctx context.Context, w Window) (billing.Tally, error) {
	if err := w.valid(); err != nil {
		return billing.Tally{}, err
	}
	var t billing.Tally
	err := db.pool.QueryRow(ctx, `
		SELECT `+ledgerTallyColumns+`
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2`, w.From, w.To).
		Scan(&t.Records, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens, &t.AmountMicro)
	if err != nil {
		return billing.Tally{}, fmt.Errorf("postgres: ledger tally: %w", err)
	}
	return t, nil
}

// UsageByGroup totals the ledger per tenant and model.
//
// The key is built by billing.GroupKey rather than by a concatenation in SQL,
// because the replica's side of the comparison builds it too and two spellings
// of the same pair would report every group as disagreeing with itself.
//
// Ordered by the key so two runs over the same rows produce the same report. The
// reconciliation sorts anyway; this makes the ordering visible in the plan
// rather than left to whatever the planner picked.
func (db *DB) UsageByGroup(ctx context.Context, w Window) ([]billing.GroupTally, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	rows, err := db.pool.Query(ctx, `
		SELECT tenant_id, model, `+ledgerTallyColumns+`
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		 GROUP BY tenant_id, model
		 ORDER BY tenant_id, model`, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("postgres: ledger groups: %w", err)
	}
	defer rows.Close()

	var out []billing.GroupTally
	for rows.Next() {
		var tenant, model string
		var t billing.Tally
		if err := rows.Scan(&tenant, &model, &t.Records, &t.PromptTokens,
			&t.CompletionTokens, &t.CachedTokens, &t.AmountMicro); err != nil {
			return nil, fmt.Errorf("postgres: scan ledger group: %w", err)
		}
		out = append(out, billing.GroupTally{Key: billing.GroupKey(tenant, model), Tally: t})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate ledger groups: %w", err)
	}
	return out, nil
}
