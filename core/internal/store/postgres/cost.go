package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// The cost pool (P8): what the fleet cost in a calendar month, what of it was
// used, and who is billed for it.
//
// Three tables, read in one pass over a period. Capacity and deployment samples
// come from the operator's own inventory reports, so nothing here asks the
// gateway to remember anything it does not already know.

type CostStore struct{ db *DB }

// NewCostStore returns a cost store.
func NewCostStore(db *DB) *CostStore { return &CostStore{db: db} }

// Heartbeat is how often an unchanged capacity is re-recorded.
//
// The operator reports every thirty seconds, which is 86,000 rows a month for a
// fleet that never changes. A sample is written when the count differs from the
// most recent one, or when the last sample is older than this — so a step change
// is captured to the second and an unchanging fleet costs a handful of rows.
//
// The change test is what stops a scale event being smeared across the interval
// that follows it, which is how a two-minute burst of capacity ends up billed
// for half an hour.
const Heartbeat = 5 * time.Minute

// RecordCapacity appends one inventory report to the time series.
//
// Cluster GPUs and per-deployment GPUs are written from the same report because
// they are the same instant: two statements could otherwise disagree about which
// snapshot they belong to, and the pool would contain GPUs no deployment held.
func (s *CostStore) RecordCapacity(ctx context.Context, report inventory.Report) error {
	at := report.Cluster.ReportedAt
	if at.IsZero() {
		return invalidf("a capacity sample needs a reported time")
	}
	return s.db.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO capacity_samples (cluster, at, gpu_count)
			SELECT $1, $2, $3 WHERE NOT EXISTS (
			  SELECT 1 FROM capacity_samples prev
			  WHERE prev.cluster = $1 AND prev.gpu_count = $3
			    AND prev.at > $2::timestamptz - $4::interval
			)`,
			report.Cluster.Name, at, clusterGPUs(report.Cluster), Heartbeat.String())
		if err != nil {
			return fmt.Errorf("postgres: capacity sample for %s: %w", report.Cluster.Name, err)
		}

		for _, d := range report.Deployments {
			if d.GPUPer <= 0 {
				continue
			}
			// No heartbeat test here: a deployment's per-replica shape changes
			// far more often than its total does, and it is the shape that
			// prices a request. Dropping those samples would re-price history
			// against whatever the shape happens to be now.
			_, err := tx.Exec(ctx, `INSERT INTO deployment_samples
				(deployment, cluster, at, gpu_per_replica, gpu_count) VALUES ($1,$2,$3,$4,$5)`,
				d.Key(), report.Cluster.Name, at, d.GPUPer, int64(d.GPUPer*max(d.Desired, 0)))
			if err != nil {
				return fmt.Errorf("postgres: deployment sample for %s: %w", d.Key(), err)
			}
		}
		return nil
	})
}

// clusterGPUs counts the GPUs a cluster currently has, ready or not.
//
// Not-ready included: a GPU that is powered on and failing still costs money,
// and a pool that quietly drops it whenever a node cordons reads as the operator
// having saved the cost of the outage.
func clusterGPUs(c inventory.Cluster) int64 {
	var total int64
	for _, n := range c.Nodes {
		total += int64(n.GPU.Count)
	}
	return total
}

// ListRates returns the declared GPU-hour prices.
func (s *CostStore) ListRates(ctx context.Context) ([]cost.Rate, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT cluster, gpu_hour_micro, currency FROM cost_rates ORDER BY cluster`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list cost rates: %w", err)
	}
	defer rows.Close()

	var out []cost.Rate
	for rows.Next() {
		var r cost.Rate
		if err := rows.Scan(&r.Cluster, &r.GPUHourMicro, &r.Currency); err != nil {
			return nil, fmt.Errorf("postgres: scan cost rate: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutRate declares what a GPU-hour costs in a cluster.
func (s *CostStore) PutRate(ctx context.Context, r cost.Rate) error {
	if r.Cluster == "" {
		return invalidf("a cost rate needs a cluster")
	}
	if r.GPUHourMicro < 0 {
		return invalidf("a cost rate cannot be negative")
	}
	if r.Currency == "" {
		r.Currency = "USD"
	}
	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO cost_rates (cluster, gpu_hour_micro, currency) VALUES ($1,$2,$3)
		 ON CONFLICT (cluster) DO UPDATE SET gpu_hour_micro = $2, currency = $3`,
		r.Cluster, r.GPUHourMicro, r.Currency)
	if err != nil {
		return wrap(err, "put cost rate for %s", r.Cluster)
	}
	return nil
}
