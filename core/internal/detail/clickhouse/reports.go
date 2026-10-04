package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// DailySpend is one row of a per-day, per-tenant, per-model report.
type DailySpend struct {
	Day              time.Time
	Tenant           string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	AmountMicro      int64
	Requests         uint64
}

// Daily reports the money and tokens spent per day, per tenant, per model.
//
// The WHERE clause filters on occurred_at, not on a day column. A day column
// would have been convenient, and it was in the first version of this file --
// but a column that only exists inside a projection makes the query compile
// nowhere, and the day is already the leading column of the ordering key, so the
// range prunes without it.
//
// This is the query the overview and the cost page issue, so its shape is the
// schema's real requirement rather than a guess.
func (s *Store) Daily(ctx context.Context, from, to time.Time, tenant string) ([]DailySpend, error) {
	q := `
		SELECT toDate(occurred_at) AS day,
		       tenant,
		       model,
		       sum(prompt_tokens)      AS prompt_tokens,
		       sum(completion_tokens) AS completion_tokens,
		       sum(cached_tokens)     AS cached_tokens,
		       sum(amount_micro)      AS amount_micro,
		       count()                AS requests
		FROM ` + s.db + `.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2
		GROUP BY day, tenant, model
		ORDER BY day, tenant, model`

	args := []any{from, to}
	if tenant != "" {
		q = `
		SELECT toDate(occurred_at) AS day,
		       tenant,
		       model,
		       sum(prompt_tokens)      AS prompt_tokens,
		       sum(completion_tokens) AS completion_tokens,
		       sum(cached_tokens)     AS cached_tokens,
		       sum(amount_micro)      AS amount_micro,
		       count()                AS requests
		FROM ` + s.db + `.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2 AND tenant = $3
		GROUP BY day, tenant, model
		ORDER BY day, tenant, model`
		args = append(args, tenant)
	}

	rows, err := s.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailySpend
	for rows.Next() {
		var d DailySpend
		if err := rows.Scan(&d.Day, &d.Tenant, &d.Model, &d.PromptTokens, &d.CompletionTokens,
			&d.CachedTokens, &d.AmountMicro, &d.Requests); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EndpointUsage is the metering audit's input for one endpoint: what the engine
// claimed and what Fleet counted, side by side.
//
// It reads through idx_endpoint rather than the ordering key, which is why that
// index exists and why endpoint is deliberately not in the key.
func (s *Store) EndpointUsage(ctx context.Context, from, to time.Time, endpoint string) (endpointTotals, error) {
	rows, err := s.Query(ctx, `
		SELECT usage_source,
		       count()                              AS n,
		       sum(completion_tokens)               AS completion_tokens,
		       countIf(truncated)                   AS truncated
		FROM `+s.db+`.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2 AND endpoint = $3
		GROUP BY usage_source`,
		from, to, endpoint)
	if err != nil {
		return endpointTotals{}, err
	}
	defer rows.Close()

	var out endpointTotals
	for rows.Next() {
		var source string
		var n, truncated uint64
		// sum() over an Int64 column stays Int64; only count() is UInt64. The
		// distinction is the driver's, not a preference -- mixing them makes the
		// scan fail rather than convert.
		var completion int64
		if err := rows.Scan(&source, &n, &completion, &truncated); err != nil {
			return endpointTotals{}, err
		}
		out.set(source, n, completion, truncated)
	}
	return out, rows.Err()
}

type endpointTotals struct {
	// Engine and Counted are kept apart because the audit's whole question is
	// whether the two agree. Summing them first would make the answer always
	// "they agree" whenever there are no engine-reported rows at all.
	EngineN, CountedN, ReservedN       uint64
	EngineOut, CountedOut, ReservedOut int64
	Truncated                          uint64
}

func (t *endpointTotals) set(source string, n uint64, completion int64, truncated uint64) {
	t.Truncated += truncated
	switch source {
	case "engine":
		t.EngineN, t.EngineOut = n, completion
	case "counted":
		t.CountedN, t.CountedOut = n, completion
	default:
		t.ReservedN, t.ReservedOut = n, completion
	}
}

// Count reports how many rows the store holds, for a reconciliation to compare
// against the ledger's own count.
func (s *Store) Count(ctx context.Context) (uint64, error) {
	var n uint64
	if err := s.QueryRow(ctx, "SELECT count() FROM "+s.db+".usage_detail").Scan(&n); err != nil {
		return 0, fmt.Errorf("clickhouse: %w", err)
	}
	return n, nil
}

// HasLedgerID reports whether a given ledger row is present, which is how a
// backfill knows where to resume and how a reconciliation names what is missing.
func (s *Store) HasLedgerID(ctx context.Context, id int64) (bool, error) {
	var n uint64
	if err := s.QueryRow(ctx,
		"SELECT count() FROM "+s.db+".usage_detail WHERE ledger_id = $1", id).Scan(&n); err != nil {
		return false, fmt.Errorf("clickhouse: %w", err)
	}
	return n > 0, nil
}
