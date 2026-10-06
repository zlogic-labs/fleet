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
	if err := s.spend(ctx, period, in.Spent); err != nil {
		return err
	}
	in.Direct = map[string]billing.Amount{}
	in.Providers = map[string]cost.ProviderCharge{}
	return s.direct(ctx, period, in.Direct, in.Providers)
}

// direct reads what commercial providers charged, by key and by provider.
//
// Filtered on provider <> ” so the fleet's own rows never appear here. That
// filter is the boundary between the two cost centres and it belongs in the
// query rather than in Go: a row that reached this function unfiltered would
// be counted as money that left the account when it was a share of a pool
// nobody paid twice for.
func (s *CostStore) direct(ctx context.Context, period cost.Period,
	byKey map[string]billing.Amount, byProvider map[string]cost.ProviderCharge) error {

	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, provider, SUM(amounts_micro), COUNT(*)
		   FROM usage_events
		  WHERE occurred_at >= $1 AND occurred_at < $2
		    AND provider <> '' AND tenant_id <> ''
		  GROUP BY 1, 2`,
		period.Start, period.End)
	if err != nil {
		return fmt.Errorf("postgres: direct spend: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			key, provider string
			micro         int64
			requests      int64
		)
		if err := rows.Scan(&key, &provider, &micro, &requests); err != nil {
			return fmt.Errorf("postgres: scan direct spend: %w", err)
		}
		byKey[key] += billing.Amount(micro)
		charge := byProvider[provider]
		charge.Amount += billing.Amount(micro)
		charge.Requests += requests
		byProvider[provider] = charge
	}
	return rows.Err()
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
//
// Rows with a provider are excluded: a request billed to a commercial API
// consumed none of this fleet's GPUs, and counting its wall clock here would
// hand a tenant pool credit for somebody else's machine time. The LATERAL join
// would mostly exclude them anyway, since a vendor endpoint has no deployment
// samples — but "mostly" is the wrong word for a billing query, and an
// operator who points a vendor route at a URL that happens to share an endpoint
// id with a deployment would get a plausible wrong answer.
func (s *CostStore) spans(ctx context.Context, period cost.Period) (map[string][]cost.Span, error) {
	const q = `SELECT u.endpoint_id, u.tenant_id, u.occurred_at, u.duration_ms, shape.gpu_per_replica
	FROM usage_events u
	JOIN LATERAL (
	  SELECT gpu_per_replica FROM deployment_samples d
	  WHERE d.deployment = u.endpoint_id AND d.at <= u.occurred_at
	  ORDER BY d.at DESC LIMIT 1
	) shape ON true
	WHERE u.occurred_at >= $1 AND u.occurred_at < $2
	  AND u.endpoint_id <> '' AND u.duration_ms > 0
	  AND u.provider = ''`

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

// spend reads what the ledger charged each key, as a weighting.
//
// Excludes provider rows, because this figure is reported next to a tenant's
// pool share as a diagnostic: "charged well above what the fleet cost" means
// the fleet's rate is too high. A vendor row in that column would say the
// opposite — that a real price exceeded a share of a pool — and the reader
// would conclude the price book was wrong when in fact the two figures are
// measuring different things.
func (s *CostStore) spend(ctx context.Context, period cost.Period, into map[string]billing.Amount) error {
	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, COALESCE(SUM(amounts_micro), 0)::bigint FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		   AND tenant_id <> '' AND provider = ''
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
