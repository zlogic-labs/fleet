package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Reading the observations a close needs, in one pass over the period.

func (s *CostStore) gather(ctx context.Context, period cost.Period, in *cost.Input) error {
	samples, err := s.capacity(ctx, period)
	if err != nil {
		return err
	}
	in.Capacity = samples
	in.Reserved = map[string][]cost.Sample{}

	rows, err := s.db.pool.Query(ctx,
		`SELECT deployment, at, gpu_count FROM deployment_samples
		 WHERE at >= $1 AND at < $2 ORDER BY deployment, at`, period.Start, period.End)
	if err != nil {
		return fmt.Errorf("postgres: deployment samples: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var at = period.Start
		var count int64
		if err := rows.Scan(&name, &at, &count); err != nil {
			return fmt.Errorf("postgres: scan deployment sample: %w", err)
		}
		in.Reserved[name] = append(in.Reserved[name], cost.Sample{At: at, Value: count})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: read deployment samples: %w", err)
	}

	spans, err := s.spans(ctx, period)
	if err != nil {
		return err
	}
	in.Spans = spans
	in.Spent = map[string]billing.Amount{}
	return s.spend(ctx, period, in.Spent)
}

func (s *CostStore) capacity(ctx context.Context, period cost.Period) (map[string][]cost.Sample, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT cluster, at, gpu_count FROM capacity_samples
		 WHERE at >= $1 AND at < $2 ORDER BY cluster, at`, period.Start, period.End)
	if err != nil {
		return nil, fmt.Errorf("postgres: capacity samples: %w", err)
	}
	defer rows.Close()

	out := map[string][]cost.Sample{}
	for rows.Next() {
		var cluster string
		var at = period.Start
		var count int64
		if err := rows.Scan(&cluster, &at, &count); err != nil {
			return nil, fmt.Errorf("postgres: scan capacity sample: %w", err)
		}
		out[cluster] = append(out[cluster], cost.Sample{At: at, Value: count})
	}
	return out, rows.Err()
}

// spans reads every request in the period as an interval, grouped by endpoint.
//
// Intervals, not pre-summed seconds. The ledger records when a request
// finished, so the occupied interval is reconstructed as
// [occurred_at - duration, occurred_at]; recording a start time instead would
// make this exact, at the price of a column that older rows would not have.
//
// Each request is priced against the shape of its deployment at the moment it
// ran, because joining today's gpu_per_replica would re-price the whole month
// every time somebody scaled a replica, and a tenant's invoice would then
// change because an operator adjusted capacity — the silent rewrite the ledger
// exists to make impossible.
//
// A request with no tenant still occupies the GPU; it is swept under the empty
// key so it counts towards utilization and towards nobody's bill.
func (s *CostStore) spans(ctx context.Context, period cost.Period) (map[string][]cost.Span, error) {
	const q = `SELECT u.endpoint_id, u.tenant_id, u.occurred_at, u.duration_ms, shape.gpu_per_replica
	FROM usage_events u
	JOIN LATERAL (
	  SELECT gpu_per_replica FROM deployment_samples d
	  WHERE d.deployment = u.endpoint_id AND d.at <= u.occurred_at
	  ORDER BY d.at DESC LIMIT 1
	) shape ON true
	WHERE u.occurred_at >= $1 AND u.occurred_at < $2
	  AND u.endpoint_id <> '' AND u.duration_ms > 0`

	rows, err := s.db.pool.Query(ctx, q, period.Start, period.End)
	if err != nil {
		return nil, fmt.Errorf("postgres: cost spans: %w", err)
	}
	defer rows.Close()

	out := map[string][]cost.Span{}
	for rows.Next() {
		var (
			endpoint, tenant string
			at               time.Time
			duration         int64
			gpus             int32
		)
		if err := rows.Scan(&endpoint, &tenant, &at, &duration, &gpus); err != nil {
			return nil, fmt.Errorf("postgres: scan cost span: %w", err)
		}
		span := cost.Span{
			From: at.Add(-time.Duration(duration) * time.Millisecond),
			To:   at,
			Key:  tenant,
			GPUs: int64(gpus),
		}
		out[endpoint] = append(out[endpoint], span)
	}
	return out, rows.Err()
}

func (s *CostStore) spend(ctx context.Context, period cost.Period, into map[string]billing.Amount) error {
	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, COALESCE(SUM(amounts_micro), 0)::bigint FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2 AND tenant_id <> ''
		 GROUP BY 1`, period.Start, period.End)
	if err != nil {
		return fmt.Errorf("postgres: spend by tenant: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var micro int64
		if err := rows.Scan(&key, &micro); err != nil {
			return fmt.Errorf("postgres: scan spend: %w", err)
		}
		into[key] = billing.Amount(micro)
	}
	return rows.Err()
}
