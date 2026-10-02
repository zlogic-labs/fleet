package postgres

import (
	"context"
	"fmt"

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

	uses, err := s.consumption(ctx, period, "tenant_id")
	if err != nil {
		return err
	}
	in.Consumption = uses
	in.Spent = make(map[string]billing.Amount, len(uses))
	for _, u := range uses {
		in.Spent[u.Key] = 0
	}

	byEndpoint, err := s.consumption(ctx, period, "endpoint_id")
	if err != nil {
		return err
	}
	in.Used = make(map[string]int64, len(byEndpoint))
	for _, u := range byEndpoint {
		in.Used[u.Key] = u.GPUSeconds
	}
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

// consumption is GPU-seconds per whatever the caller named, with each request
// priced against the shape of its deployment at the moment it ran.
//
// The as-of lookup matters: joining today's gpu_per_replica would re-price the
// whole month every time somebody scales a replica, so a tenant's invoice would
// change because an operator adjusted capacity — which is exactly the kind of
// silent rewrite the ledger is built to make impossible.
func (s *CostStore) consumption(ctx context.Context, period cost.Period, groupBy string) ([]cost.Use, error) {
	const q = `SELECT %[1]s AS scope,
	  COALESCE(SUM((u.duration_ms::bigint / 1000.0) * shape.gpu_per_replica), 0)::bigint
	FROM usage_events u
	JOIN LATERAL (
	  SELECT gpu_per_replica FROM deployment_samples d
	  WHERE d.deployment = u.endpoint_id AND d.at <= u.occurred_at
	  ORDER BY d.at DESC LIMIT 1
	) shape ON true
	WHERE u.occurred_at >= $1 AND u.occurred_at < $2 AND u.%[1]s <> ''
	GROUP BY 1 ORDER BY 1`

	rows, err := s.db.pool.Query(ctx, fmt.Sprintf(q, groupBy), period.Start, period.End)
	if err != nil {
		return nil, fmt.Errorf("postgres: consumption by %s: %w", groupBy, err)
	}
	defer rows.Close()

	var out []cost.Use
	for rows.Next() {
		var u cost.Use
		if err := rows.Scan(&u.Key, &u.GPUSeconds); err != nil {
			return nil, fmt.Errorf("postgres: scan consumption: %w", err)
		}
		out = append(out, u)
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
