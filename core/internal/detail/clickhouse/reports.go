package clickhouse

import (
	"context"
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
// This is the shape a replica-backed report needs, and it is written against a
// real server rather than composed from what the schema is supposed to say: it
// once filtered on a `day` column that only existed inside a projection, so the
// query did not compile against the table at all — and that failed only when run
// against a server, never in a test that stopped at building a string.
//
// Nothing in the console reads it yet. The overview and the cost page read the
// ledger, which is the authoritative store for money; this exists for the
// questions that are the wrong shape for a relational table.
func (s *Store) Daily(ctx context.Context, from, to time.Time, tenant string) ([]DailySpend, error) {
	// One query with an optional predicate rather than two literals that have to
	// be kept identical: the two copies this replaces were already the same
	// twelve lines twice, and a column added to one of them would have made the
	// scoped and unscoped reports count different things.
	//
	// cached_tokens is COALESCEd for the same reason the ledger's spend queries
	// are: an engine that reports totals without a breakdown has said nothing,
	// and SUM over a group where every row said nothing is NULL rather than
	// zero. Scanning NULL into an int fails the report, so a month served
	// entirely by such an engine would answer with an error where it should
	// answer with a number.
	q := `
		SELECT toDate(occurred_at) AS day,
		       tenant,
		       model,
		       sum(prompt_tokens)      AS prompt_tokens,
		       sum(completion_tokens) AS completion_tokens,
		       coalesce(sum(cached_tokens), 0) AS cached_tokens,
		       sum(amount_micro)      AS amount_micro,
		       count()                AS requests
		FROM ` + s.db + `.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2`
	args := []any{from, to}
	if tenant != "" {
		q += ` AND tenant = $3`
		args = append(args, tenant)
	}
	q += ` GROUP BY day, tenant, model ORDER BY day, tenant, model`

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

// A count of the whole table and a per-id membership test used to live here,
// with a comment saying a reconciliation would use them. It does not: the
// comparison is per window and per group (reconcile.go), and asking after one id
// at a time would be a full scan of the table per id, because ledger_id is not
// in the ordering key. Both were removed rather than left as a promise.
