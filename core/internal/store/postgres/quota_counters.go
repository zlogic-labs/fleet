package postgres

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"time"
)

func sum(ctx context.Context, q querier, scope string, rule quota.Rule, now time.Time) (int64, error) {
	spent, reserved := rule.Dimension.Columns()
	if spent == "" {
		return 0, fmt.Errorf("postgres: unknown dimension %q", rule.Dimension)
	}
	stmt := fmt.Sprintf(`
		SELECT COALESCE(SUM(%[1]s), 0) + COALESCE(SUM(%[2]s), 0)
		  FROM spend_counters
		 WHERE scope = $1 AND bucket_start > $2`, spent, reserved)
	from := quota.BucketStart(now, rule.Resolution()).Add(-rule.Window)

	var total int64
	if err := q.QueryRow(ctx, stmt, scope, from).Scan(&total); err != nil {
		return 0, fmt.Errorf("postgres: sum %s %s: %w", scope, rule.Dimension, err)
	}
	return total, nil
}

// whenFree reports when the oldest spending leaves the window.
//
// The earliest bucket holding anything, not the window's start: a tenant whose
// traffic began ten minutes ago has ten minutes of spending to wait out, and
// quoting the full window would tell them to come back far later than they
// need to. An empty window frees everything at once, at the end.
func whenFree(ctx context.Context, q querier, scope string, rule quota.Rule, now time.Time) (time.Time, error) {
	spent, reserved := rule.Dimension.Columns()
	stmt := fmt.Sprintf(`
		SELECT MIN(bucket_start)
		  FROM spend_counters
		 WHERE scope = $1 AND bucket_start > $2 AND (%[1]s + %[2]s) > 0`,
		spent, reserved)
	from := quota.BucketStart(now, rule.Resolution()).Add(-rule.Window)

	var oldest *time.Time
	if err := q.QueryRow(ctx, stmt, scope, from).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("postgres: find the oldest spend for %s: %w", scope, err)
	}
	if oldest == nil {
		return now.Add(rule.Window), nil
	}
	return oldest.Add(rule.Window), nil
}

// write applies one set of deductions to the counters.
//
// Reserving adds to the reserved column; settling takes the reservation back
// out and adds the real figure to spent. One statement shape for both, because
// the alternative is two places that have to stay in step and a settlement that
// silently stops releasing.
// reserved is what to take out of the reserved column and spent is what to add
// to the spent column. They are different numbers on both paths — a reservation
// releases what it took and records what it actually cost — and collapsing them
// into one is how a reservation either never comes back or comes back for the
// wrong amount.
func write(ctx context.Context, tx pgx.Tx, rules []quota.Rule,
	committed, settled []int64, now time.Time, settling bool) error {

	for i, rule := range rules {
		spent, reserved := rule.Dimension.Columns()
		if spent == "" {
			return fmt.Errorf("postgres: unknown dimension %q", rule.Dimension)
		}
		deltaReserved, deltaSpent := committed[i], int64(0)
		if settling {
			deltaReserved, deltaSpent = -committed[i], settled[i]
		}
		stmt := fmt.Sprintf(`
			INSERT INTO spend_counters (scope, bucket_start, %[1]s, %[2]s)
			VALUES ($1, $2, GREATEST(0, $3), GREATEST(0, $4))
			ON CONFLICT (scope, bucket_start) DO UPDATE
			   SET %[1]s = GREATEST(0, spend_counters.%[1]s + $3),
			       %[2]s = GREATEST(0, spend_counters.%[2]s + $4)`,
			reserved, spent)
		bucket := quota.BucketStart(now, rule.Resolution())
		if _, err := tx.Exec(ctx, stmt, rules[i].ScopeKey(), bucket, deltaReserved, deltaSpent); err != nil {
			return fmt.Errorf("postgres: write %s %s: %w", rules[i].ScopeKey(), rule.Dimension, err)
		}
	}
	return nil
}

// Settle replaces a reservation with the real charge.
//
// It cannot return an error: the response is already on the wire. A miss here
// leaves the reservation outstanding, which bucket pruning or a rebuild from the
// ledger clears — the ledger stays the authority, and this is the one place a
// counter being wrong is recoverable.
