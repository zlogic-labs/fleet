package postgres

import (
	"context"
	"fmt"
	"time"
)

// Spend queries.
//
// The reports the product asks for, in the order it asks for them. Each is one
// statement: a report that needs a round trip per row is a report the console
// cannot render, and the alternative — loading events and aggregating in Go —
// is how a dashboard starts timing out at the end of a busy month.

// Spend is a rolled-up total.
type Spend struct {
	// Key is whatever the query grouped by: a tenant, a project, a model, or an
	// endpoint. One field rather than four report types, because the console
	// renders them all as the same table with a different column header.
	Key string
	// UnitsMicro is the spend in millionths of a quota unit, matching the
	// ledger's amounts_micro. Not a float: a month of a busy tenant is a large
	// number of micro-units and a float64 would lose the cents.
	UnitsMicro int64
	Requests   int64
	// PromptTokens and the rest are the raw engine-reported totals, so a
	// report can show tokens per unit of money without joining anything.
	PromptTokens     int64
	CompletionTokens int64
	// CachedTokens is reported separately because the platform's margin comes
	// from prefix-cache hit rate (§6): a tenant whose cost is mostly cache hits
	// is being served well, and hiding that inside PromptTokens would make it
	// invisible.
	CachedTokens int64
	// Estimated is how many of Requests had no engine-reported usage. A
	// non-zero value means the bill is partly made of counted answers or
	// max_tokens guesses, which is a fact the operator needs before they trust
	// the total.
	Estimated int64
	// Sources breaks Estimated down by what the figure was measured from.
	// Without that, "12 estimated requests" cannot be distinguished from "12
	// requests measured by the gateway", which are very different things to
	// trust: the first is arithmetic, the second is the fleet's metering
	// accuracy.
	Sources map[string]int64
}

// Window bounds a report.
//
// Half-open [From, To), because a report that includes both endpoints double
// counts the boundary instant — the same bug a sliding-window rate limit has to
// avoid, and the same reason both use the same convention.
type Window struct {
	From time.Time
	To   time.Time
}

// valid reports whether the window is usable.
func (w Window) valid() error {
	if w.From.IsZero() || w.To.IsZero() {
		return fmt.Errorf("postgres: report window needs both ends")
	}
	if !w.To.After(w.From) {
		return fmt.Errorf("postgres: window end %s is not after start %s", w.To, w.From)
	}
	return nil
}

// SpendByTenant totals one tenant's spend across its projects.
//
// Unattributed spend (a NULL project_id) is folded into the tenant's own row
// rather than dropped, which is why the query COALESCEs: a report that silently
// omits rows is worse than one that labels them poorly.
func (db *DB) SpendByTenant(ctx context.Context, tenant string, w Window) ([]Spend, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	const q = `
		SELECT COALESCE(project_id, ''), SUM(amounts_micro), COUNT(*),
		       SUM(prompt_tokens), SUM(completion_tokens), COALESCE(SUM(cached_tokens), 0),
		       COUNT(*) FILTER (WHERE NOT usage_known)
		  FROM usage_events
		 WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		 GROUP BY COALESCE(project_id, '')
		 ORDER BY SUM(amounts_micro) DESC`
	return db.spend(ctx, q, tenant, w.From, w.To)
}

// SpendByScope totals spend grouped by tenant and project, across every tenant.
//
// This is SpendByTenant without the filter, and it exists because
// SpendByTenant("") does not mean "all tenants": its predicate is an equality
// test, so the empty string matches only genuinely unattributed rows. An
// operator-level report needs the unfiltered grouping, and the only honest way
// to get it is a query that does not have the predicate at all.
//
// The returned Key is "tenant/project", or bare "tenant" for unattributed
// spend. It is the ledger's own scope string rather than a second format, so a
// row in this report joins to the same key a request was written under.
func (db *DB) SpendByScope(ctx context.Context, w Window) ([]Spend, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	const q = `
		SELECT CASE WHEN project_id IS NULL OR project_id = ''
		            THEN tenant_id ELSE project_id END AS scope,
		       SUM(amounts_micro), COUNT(*),
		       SUM(prompt_tokens), SUM(completion_tokens), COALESCE(SUM(cached_tokens), 0),
		       COUNT(*) FILTER (WHERE NOT usage_known)
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		 GROUP BY 1
		 ORDER BY SUM(amounts_micro) DESC`
	return db.spend(ctx, q, w.From, w.To)
}

// SpendByModel totals spend grouped by model, across every tenant.
//
// The model is the resolved one recorded on the ledger, so this answers "what
// is actually costing us" rather than "what did clients type".
func (db *DB) SpendByModel(ctx context.Context, w Window) ([]Spend, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	const q = `
		SELECT model, SUM(amounts_micro), COUNT(*),
		       SUM(prompt_tokens), SUM(completion_tokens), COALESCE(SUM(cached_tokens), 0),
		       COUNT(*) FILTER (WHERE NOT usage_known)
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		 GROUP BY model
		 ORDER BY SUM(amounts_micro) DESC`
	return db.spend(ctx, q, w.From, w.To)
}

// EstimatedUsage lists events charged from an estimate and not yet reconciled.
//
// This is the P6 worklist: each row was billed at max_tokens because the engine
// reported no usage, and every one of them is money Fleet over-charged until
// someone corrects it. Bounded by limit because this is a queue, not a report —
// an operator pages through it and the next page exists.
func (db *DB) EstimatedUsage(ctx context.Context, w Window, limit int) ([]int64, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1000
	}
	const q = `
		SELECT id FROM usage_events
		 WHERE NOT usage_known AND occurred_at >= $1 AND occurred_at < $2
		 ORDER BY occurred_at
		 LIMIT $3`
	rows, err := db.pool.Query(ctx, q, w.From, w.To, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list estimated usage: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: scan estimated usage id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// spend runs one grouped aggregation and scans it.
//
// The cached-token column is COALESCEd in the queries above, and that is not
// tidiness. It is nullable because an engine that reports totals without a
// breakdown has said nothing, and SUM over a group where every row said
// nothing is NULL rather than 0. Scanning NULL into an int fails the whole
// report, so a month served entirely by engines that omit the breakdown -- a
// real and common month -- would return an error instead of a number.
//
// Shared so the three reports cannot drift in what they count: same columns,
// same FILTER for estimates, same order. A report that counts estimates
// differently from the others is how a total stops matching its parts.
func (db *DB) spend(ctx context.Context, q string, args ...any) ([]Spend, error) {
	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: spend report: %w", err)
	}
	defer rows.Close()

	var out []Spend
	for rows.Next() {
		var s Spend
		if err := rows.Scan(&s.Key, &s.UnitsMicro, &s.Requests,
			&s.PromptTokens, &s.CompletionTokens, &s.CachedTokens, &s.Estimated); err != nil {
			return nil, fmt.Errorf("postgres: scan spend row: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate spend rows: %w", err)
	}
	return out, nil
}
