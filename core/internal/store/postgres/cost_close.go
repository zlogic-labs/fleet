package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Gathering a period and committing the result of closing it.

// ClosePeriod computes and stores a period's cost report.
//
// The report is written only after cost.Close has accepted it, so a period that
// cannot be accounted for leaves no row — a half-written invoice is worse than
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

	rep, err := cost.Close(in)
	if err != nil {
		return cost.Report{}, err
	}
	if err := s.db.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cost_periods
			(period, currency, priced, pool_micro, busy_micro, idle_micro,
			 idle_percent, coverage_percent, pool_gpu_seconds)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			rep.Period, rep.Currency, rep.Priced, int64(rep.Pool), int64(rep.Busy), int64(rep.Idle),
			rep.IdlePct, rep.CoveragePercent, rep.PoolSeconds)
		if err != nil {
			return wrap(err, "close period %s", rep.Period)
		}
		for _, t := range rep.Tenants {
			if _, err := tx.Exec(ctx, `INSERT INTO cost_allocations
				(period, scope, gpu_seconds, share, amount_micro, usage_micro)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				rep.Period, t.Key, t.GPUSeconds, t.Share, int64(t.Amount), int64(t.UsageMicro)); err != nil {
				return fmt.Errorf("postgres: allocation for %s: %w", t.Key, err)
			}
		}
		return nil
	}); err != nil {
		return cost.Report{}, err
	}
	return rep, nil
}

// GetPeriod reads a closed period back, allocations included.
func (s *CostStore) GetPeriod(ctx context.Context, period string) (cost.Report, error) {
	rep := cost.Report{Period: period}
	err := s.db.pool.QueryRow(ctx, `SELECT currency, priced, pool_micro, busy_micro, idle_micro,
		idle_percent, coverage_percent, pool_gpu_seconds FROM cost_periods WHERE period = $1`, period).
		Scan(&rep.Currency, &rep.Priced, &rep.Pool, &rep.Busy, &rep.Idle,
			&rep.IdlePct, &rep.CoveragePercent, &rep.PoolSeconds)
	if err != nil {
		return cost.Report{}, wrapNotFound(err, "period %s is not closed", period)
	}
	if p, err := cost.ParsePeriod(period); err == nil {
		rep.From, rep.To = p.Start, p.End
	}

	rows, err := s.db.pool.Query(ctx,
		`SELECT scope, gpu_seconds, share, amount_micro, usage_micro
		 FROM cost_allocations WHERE period = $1 ORDER BY scope`, period)
	if err != nil {
		return cost.Report{}, fmt.Errorf("postgres: get allocations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t cost.Tenant
		if err := rows.Scan(&t.Key, &t.GPUSeconds, &t.Share, &t.Amount, &t.UsageMicro); err != nil {
			return cost.Report{}, fmt.Errorf("postgres: scan allocation: %w", err)
		}
		rep.Tenants = append(rep.Tenants, t)
	}
	if err := rows.Err(); err != nil {
		return cost.Report{}, fmt.Errorf("postgres: read allocations: %w", err)
	}
	var sum billing.Amount
	for _, t := range rep.Tenants {
		sum += t.Amount
	}
	rep.Allocated = sum
	return rep, nil
}

// ListPeriods returns closed periods, newest first.
func (s *CostStore) ListPeriods(ctx context.Context) ([]cost.Report, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT period, currency, priced, pool_micro, busy_micro, idle_micro, idle_percent
		 FROM cost_periods ORDER BY period DESC LIMIT 60`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list periods: %w", err)
	}
	defer rows.Close()

	var out []cost.Report
	for rows.Next() {
		var r cost.Report
		if err := rows.Scan(&r.Period, &r.Currency, &r.Priced, &r.Pool, &r.Busy, &r.Idle, &r.IdlePct); err != nil {
			return nil, fmt.Errorf("postgres: scan period: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
