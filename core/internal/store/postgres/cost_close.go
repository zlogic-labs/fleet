package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Closing a period, and the corrections that follow it.

// ErrFrozen reports a period that can no longer be recomputed.
//
// It is a distinct error because the operator's next move is not "try again":
// the month after it has been invoiced, so the correction has to leave Fleet as
// a credit note or a write-off. A 409 that says "already closed" would send them
// looking for a missing button.
type ErrFrozen struct {
	Period string
	After  string
}

func (e *ErrFrozen) Error() string {
	return fmt.Sprintf("postgres: period %s is frozen: %s has been closed, so a correction has no month to land in",
		e.Period, e.After)
}

// ClosePeriod computes and stores a period's cost report.
//
// Recomputing an existing period is allowed until a later one is closed, and
// books the difference to the month immediately after it. That is the whole of
// Fleet's cross-period rule: there is no balance to roll forward, because a
// period apportions a pool that has already been paid for and a request is
// reserved and accounted for together, so no tenant can owe forward. What a
// tenant can be short-changed by is a month closed before all of its
// observations arrived — and that difference is collected, not written back.
//
// The report is written only after cost.Close has accepted it, so a period that
// cannot be accounted for leaves no row: a half-written invoice is worse than
// none, because it looks authoritative.
func (s *CostStore) ClosePeriod(ctx context.Context, period cost.Period, minCoverage float64) (cost.Report, error) {
	rates, err := s.ListRates(ctx)
	if err != nil {
		return cost.Report{}, err
	}
	in := cost.Input{Period: period, Rates: rates, MinCoverage: minCoverage}
	if err := s.gather(ctx, period, &in); err != nil {
		return cost.Report{}, err
	}
	in.Adjustments, err = s.adjustmentsInto(ctx, period.String())
	if err != nil {
		return cost.Report{}, err
	}

	previous, err := s.latestRevision(ctx, period)
	if err != nil {
		return cost.Report{}, err
	}
	in.Revision = 1
	if previous != nil {
		if after, err := s.laterClosedPeriod(ctx, period); err != nil {
			return cost.Report{}, err
		} else if after != "" {
			return cost.Report{}, &ErrFrozen{Period: period.String(), After: after}
		}
		in.Revision = previous.Revision + 1
	}

	rep, err := cost.Close(in)
	if err != nil {
		return cost.Report{}, err
	}
	if previous != nil {
		amended, err := s.recordAmendments(ctx, period, cost.Diff(*previous, rep))
		if err != nil {
			return cost.Report{}, err
		}
		rep.Amended = amended
	}
	if err := s.store(ctx, rep); err != nil {
		return cost.Report{}, err
	}
	return rep, nil
}

// store writes a report: the period's totals and its allocation rows.
func (s *CostStore) store(ctx context.Context, rep cost.Report) error {
	return s.db.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cost_periods
			(period, currency, priced, pool_micro, busy_micro, idle_micro,
			 idle_percent, coverage_percent, pool_gpu_seconds, revision)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (period) DO UPDATE SET
				currency = EXCLUDED.currency, priced = EXCLUDED.priced,
				pool_micro = EXCLUDED.pool_micro, busy_micro = EXCLUDED.busy_micro,
				idle_micro = EXCLUDED.idle_micro, idle_percent = EXCLUDED.idle_percent,
				coverage_percent = EXCLUDED.coverage_percent,
				pool_gpu_seconds = EXCLUDED.pool_gpu_seconds, revision = EXCLUDED.revision`,
			rep.Period, rep.Currency, rep.Priced, int64(rep.Pool), int64(rep.Busy), int64(rep.Idle),
			rep.IdlePct, rep.CoveragePercent, rep.PoolSeconds, rep.Revision)
		if err != nil {
			return wrap(err, "close period %s", rep.Period)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cost_allocations WHERE period = $1`, rep.Period); err != nil {
			return fmt.Errorf("postgres: clear allocations of %s: %w", rep.Period, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cost_providers WHERE period = $1`, rep.Period); err != nil {
			return fmt.Errorf("postgres: clear provider charges of %s: %w", rep.Period, err)
		}
		for _, t := range rep.Tenants {
			if _, err := tx.Exec(ctx, `INSERT INTO cost_allocations
				(period, scope, gpu_seconds, share, amount_micro, usage_micro,
				 revision, adjustment_micro, direct_micro)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				rep.Period, t.Key, t.GPUSeconds, t.Share, int64(t.Amount), int64(t.UsageMicro),
				rep.Revision, int64(t.Adjustment), int64(t.Direct)); err != nil {
				return fmt.Errorf("postgres: allocation for %s: %w", t.Key, err)
			}
		}
		for _, p := range rep.Providers {
			if _, err := tx.Exec(ctx, `INSERT INTO cost_providers
				(period, provider, amount_micro, requests)
				VALUES ($1,$2,$3,$4)`,
				rep.Period, p.Provider, int64(p.Amount), p.Requests); err != nil {
				return fmt.Errorf("postgres: provider charge for %s: %w", p.Provider, err)
			}
		}
		return nil
	})
}

// recordAmendments books the movement a revision caused into the following month.
//
// The carrying period is the one immediately after the corrected one rather than
// the month the fix happened in: a correction found in January should still land
// in October, because October is the bill that turned out to be wrong. Two
// revisions of the same period accumulate into the same month, and the carrying
// month is by definition not closed yet — ClosePeriod refuses to revise once one
// is, which is what guarantees this has somewhere to go.
func (s *CostStore) recordAmendments(ctx context.Context, period cost.Period, deltas []cost.Adjustment) ([]cost.Amendment, error) {
	if len(deltas) == 0 {
		return nil, nil
	}
	next := cost.NewPeriod(period.Start.AddDate(0, 1, 1))

	err := s.db.inTx(ctx, func(tx pgx.Tx) error {
		for _, d := range deltas {
			const q = `INSERT INTO cost_adjustments
				(period, for_period, scope, gpu_seconds, amount_micro)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (period, for_period, scope) DO UPDATE SET
					gpu_seconds = cost_adjustments.gpu_seconds + EXCLUDED.gpu_seconds,
					amount_micro = cost_adjustments.amount_micro + EXCLUDED.amount_micro`
			if _, err := tx.Exec(ctx, q, next.String(), period.String(), d.Scope,
				d.GPUSeconds, int64(d.Amount)); err != nil {
				return fmt.Errorf("postgres: amendment %s->%s: %w", period, next, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]cost.Amendment, 0, len(deltas))
	for _, d := range deltas {
		d.ForPeriod = next.String()
		out = append(out, d)
	}
	return out, nil
}

// latestRevision reads a period as it stands, or nil when it was never closed.
func (s *CostStore) latestRevision(ctx context.Context, period cost.Period) (*cost.Report, error) {
	rep, err := s.GetPeriod(ctx, period.String())
	if err != nil {
		if NotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &rep, nil
}

// laterClosedPeriod returns the earliest closed period after this one, or "".
func (s *CostStore) laterClosedPeriod(ctx context.Context, period cost.Period) (string, error) {
	var after string
	err := s.db.pool.QueryRow(ctx,
		`SELECT period FROM cost_periods WHERE period > $1 ORDER BY period LIMIT 1`,
		period.String()).Scan(&after)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: look for a later closed period: %w", err)
	}
	return after, nil
}
