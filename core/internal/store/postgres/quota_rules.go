package postgres

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"time"
)

func (q *Quota) Settle(ctx context.Context, r quota.Reservation, actual quota.Estimate) {
	_ = q.db.inTx(ctx, func(tx pgx.Tx) error {
		return write(ctx, tx, r.Rules(),
			r.Committed(), measures(r.Rules(), actual), r.At(), true)
	})
}

// measures is the real figure per rule, so a settlement records the actual
// charge against every rule it had reserved for — including rules the estimate
// over-reserved and rules it did not.
func measures(rules []quota.Rule, actual quota.Estimate) []int64 {
	out := make([]int64, len(rules))
	for i, rule := range rules {
		out[i] = actual.Measure(rule.Dimension)
	}
	return out
}

// Used reports consumption per dimension over each rule's own window.
func (q *Quota) Used(ctx context.Context, scope ratelimit.Scope) (map[quota.Dimension]int64, error) {
	rules, err := q.rulesFor(ctx, scope)
	if err != nil {
		return nil, err
	}
	// A dimension can be ruled at both levels, and the two answers are
	// different numbers: the tenant's covers every project it owns, the
	// project's covers this one. A map keyed by dimension can only carry one,
	// so the project's wins — it is the figure about the scope actually asked
	// for, whereas the tenant's total is about a scope that was not. A caller
	// wanting the envelope asks for the tenant.
	out := map[quota.Dimension]int64{}
	owner := map[quota.Dimension]quota.ScopeKind{}
	for _, rule := range rules {
		if seen, ok := owner[rule.Dimension]; ok && seen == quota.KindProject {
			continue
		}
		used, err := sum(ctx, q.db.pool, rule.ScopeKey(), rule, time.Now())
		if err != nil {
			return nil, err
		}
		out[rule.Dimension] = used
		owner[rule.Dimension] = rule.ScopeKind
	}
	return out, nil
}

// PutRule stores a rule, replacing any rule on the same scope, dimension and
// window.
//
// A replace rather than an insert-because-absent, because the uniqueness index
// is the rule "one cap per dimension per window" and a caller that met that
// error would have to know to re-read first.
func (q *Quota) PutRule(ctx context.Context, r quota.Rule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	const stmt = `
		INSERT INTO budget_rules
			(id, scope_kind, scope_id, dimension, limit_value,
			 window_seconds, resolution_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (scope_kind, scope_id, dimension, window_seconds) DO UPDATE
		   SET limit_value = EXCLUDED.limit_value,
		       resolution_seconds = EXCLUDED.resolution_seconds`
	_, err := q.db.pool.Exec(ctx, stmt, ruleID(r), r.ScopeKind, r.ScopeID,
		r.Dimension, r.Limit, int64(r.Window/time.Second), int64(r.Resolution()/time.Second))
	if err != nil {
		return fmt.Errorf("postgres: put budget rule: %w", err)
	}
	return nil
}

// DeleteRule removes one cap. A scope with no rows is unlimited, which is the
// natural way to say "no cap" and needs no tombstone.
func (q *Quota) DeleteRule(ctx context.Context, kind quota.ScopeKind, id string, d quota.Dimension, window time.Duration) error {
	tag, err := q.db.pool.Exec(ctx,
		`DELETE FROM budget_rules
		  WHERE scope_kind = $1 AND scope_id = $2 AND dimension = $3 AND window_seconds = $4`,
		kind, id, d, int64(window/time.Second))
	if err != nil {
		return fmt.Errorf("postgres: delete budget rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: no such budget rule on %s %s", kind, id)
	}
	return nil
}

// Rules lists a scope's rules, for the control plane.
func (q *Quota) Rules(ctx context.Context, kind quota.ScopeKind, id string) ([]quota.Rule, error) {
	rows, err := q.db.pool.Query(ctx,
		`SELECT scope_kind, scope_id, dimension, limit_value, window_seconds
		   FROM budget_rules WHERE scope_kind = $1 AND scope_id = $2
		  ORDER BY dimension, window_seconds`, kind, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: list budget rules: %w", err)
	}
	defer rows.Close()

	var out []quota.Rule
	for rows.Next() {
		var (
			k, rid, d  string
			limit, sec int64
		)
		if err := rows.Scan(&k, &rid, &d, &limit, &sec); err != nil {
			return nil, fmt.Errorf("postgres: scan a budget rule: %w", err)
		}
		out = append(out, quota.Rule{
			ScopeKind: quota.ScopeKind(k), ScopeID: rid,
			Dimension: quota.Dimension(d), Limit: limit,
			Window: time.Duration(sec) * time.Second,
		})
	}
	return out, rows.Err()
}

// Prune drops buckets no rule can still read.
//
// Without it the table grows forever, and it grows on the write path of every
// request. Anything older than the longest window is invisible to every check,
// so deleting it cannot change an answer.
func (q *Quota) Prune(ctx context.Context) (int64, error) {
	var horizon time.Time
	err := q.db.pool.QueryRow(ctx,
		`SELECT now() - make_interval(secs => MAX(window_seconds)) FROM budget_rules`).
		Scan(&horizon)
	if err != nil && !isNoRows(err) {
		return 0, fmt.Errorf("postgres: find the longest window: %w", err)
	}
	tag, err := q.db.pool.Exec(ctx,
		`DELETE FROM spend_counters WHERE bucket_start < $1`, horizon)
	if err != nil {
		return 0, fmt.Errorf("postgres: prune spend counters: %w", err)
	}
	return tag.RowsAffected(), nil
}

func ruleID(r quota.Rule) string {
	return fmt.Sprintf("%s/%s/%s/%d", r.ScopeKind, r.ScopeID, r.Dimension, int64(r.Window/time.Second))
}

// isNoRows reports the "the query matched nothing" case, which for the
// longest-window lookup means "no rules exist, so nothing needs pruning".
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
