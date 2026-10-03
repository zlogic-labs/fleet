package postgres

import (
	"context"
	"fmt"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Reading closed periods back.

// GetPeriod reads a closed period, allocations and corrections included.
func (s *CostStore) GetPeriod(ctx context.Context, period string) (cost.Report, error) {
	rep := cost.Report{Period: period, Revision: 1}
	err := s.db.pool.QueryRow(ctx, `SELECT currency, priced, pool_micro, busy_micro, idle_micro,
		idle_percent, coverage_percent, pool_gpu_seconds, revision
		FROM cost_periods WHERE period = $1`, period).
		Scan(&rep.Currency, &rep.Priced, &rep.Pool, &rep.Busy, &rep.Idle,
			&rep.IdlePct, &rep.CoveragePercent, &rep.PoolSeconds, &rep.Revision)
	if err != nil {
		return cost.Report{}, wrapNotFound(err, "period %s is not closed", period)
	}
	if p, err := cost.ParsePeriod(period); err == nil {
		rep.From, rep.To = p.Start, p.End
	}

	rows, err := s.db.pool.Query(ctx,
		`SELECT scope, gpu_seconds, share, amount_micro, usage_micro, adjustment_micro
		 FROM cost_allocations WHERE period = $1 ORDER BY scope`, period)
	if err != nil {
		return cost.Report{}, fmt.Errorf("postgres: get allocations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t cost.Tenant
		if err := rows.Scan(&t.Key, &t.GPUSeconds, &t.Share, &t.Amount, &t.UsageMicro, &t.Adjustment); err != nil {
			return cost.Report{}, fmt.Errorf("postgres: scan allocation: %w", err)
		}
		rep.Tenants = append(rep.Tenants, t)
	}
	if err := rows.Err(); err != nil {
		return cost.Report{}, fmt.Errorf("postgres: read allocations: %w", err)
	}

	rep.Adjustments, err = s.adjustmentsInto(ctx, period)
	if err != nil {
		return cost.Report{}, err
	}
	for _, adj := range rep.Adjustments {
		rep.AdjustmentTotal += adj.Amount
	}
	rep.Amended, err = s.amendedBy(ctx, period)
	if err != nil {
		return cost.Report{}, err
	}
	var sum billing.Amount
	for _, t := range rep.Tenants {
		sum += t.Amount
	}
	rep.Allocated = sum
	return rep, nil
}

// ListPeriods returns closed periods, newest first.
//
// It returns the same Report type as a single close because there is one shape
// to learn. Every column the type carries is selected here: a list that filled
// the summary fields and left coverage at zero made the console warn that a
// fully observed month was 0% observed, which on an immutable invoice is worse
// than showing nothing.
func (s *CostStore) ListPeriods(ctx context.Context) ([]cost.Report, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT period, currency, priced, pool_micro, busy_micro, idle_micro, idle_percent,
		        coverage_percent, pool_gpu_seconds, revision
		   FROM cost_periods ORDER BY period DESC LIMIT 60`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list periods: %w", err)
	}
	defer rows.Close()

	var out []cost.Report
	for rows.Next() {
		var r cost.Report
		if err := rows.Scan(&r.Period, &r.Currency, &r.Priced, &r.Pool, &r.Busy, &r.Idle,
			&r.IdlePct, &r.CoveragePercent, &r.PoolSeconds, &r.Revision); err != nil {
			return nil, fmt.Errorf("postgres: scan period: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// adjustmentsInto reads the corrections booked into a period.
func (s *CostStore) adjustmentsInto(ctx context.Context, period string) ([]cost.Adjustment, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT scope, for_period, gpu_seconds, amount_micro
		   FROM cost_adjustments WHERE period = $1 ORDER BY for_period, scope`, period)
	if err != nil {
		return nil, fmt.Errorf("postgres: list adjustments: %w", err)
	}
	defer rows.Close()

	var out []cost.Adjustment
	for rows.Next() {
		var a cost.Adjustment
		if err := rows.Scan(&a.Scope, &a.ForPeriod, &a.GPUSeconds, &a.Amount); err != nil {
			return nil, fmt.Errorf("postgres: scan adjustment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// amendedBy lists the movements this period's revision caused, and which month
// is collecting each of them. Read from the other end of the trail so an
// operator looking at a disputed month can see that it was revised, and where
// the difference went, without having to know which month discovered it.
func (s *CostStore) amendedBy(ctx context.Context, period string) ([]cost.Amendment, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT period, scope, gpu_seconds, amount_micro
		   FROM cost_adjustments WHERE for_period = $1 ORDER BY period, scope`, period)
	if err != nil {
		return nil, fmt.Errorf("postgres: list amendments: %w", err)
	}
	defer rows.Close()

	var out []cost.Amendment
	for rows.Next() {
		var a cost.Amendment
		if err := rows.Scan(&a.ForPeriod, &a.Scope, &a.GPUSeconds, &a.Amount); err != nil {
			return nil, fmt.Errorf("postgres: scan amendment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
