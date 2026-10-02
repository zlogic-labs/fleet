package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// The as-of join is the reason usage_events stores the endpoint rather than
// Fleet re-deriving capacity from today's deployment shape. These two tests are
// the ones that would catch a regression to a plain join.

func TestAPriceRequestIsPricedAgainstTheShapeItRanOn(t *testing.T) {
	s := newCostStore(t)
	p := period(t)

	// Two GPUs per replica when the request ran...
	shape(t, s, "fleet/llama", 2, p.Start.Add(time.Hour))
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 500_000, p.Start.Add(2*time.Hour))
	// ...and four afterwards. Joining today's shape would quadruple the cost of
	// the request that ran an hour ago.
	shape(t, s, "fleet/llama", 4, p.Start.Add(3*time.Hour))

	seconds := consumptionFor(t, s, p, "acme")
	if want := int64(3600 * 2); seconds != want {
		t.Fatalf("priced %d GPU-seconds, want %d (the shape at request time)", seconds, want)
	}
}

func TestARequestBeforeAnySampleCostsNothingRatherThanGuessing(t *testing.T) {
	s := newCostStore(t)
	p := period(t)
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 500_000, p.Start.Add(time.Hour))
	shape(t, s, "fleet/llama", 2, p.Start.Add(4*time.Hour))

	if got := consumptionFor(t, s, p, "acme"); got != 0 {
		t.Fatalf("priced %d GPU-seconds against a shape Fleet had not seen yet", got)
	}
}

func shape(t *testing.T, s *CostStore, deployment string, gpuPer int, at time.Time) {
	t.Helper()
	_, err := s.db.pool.Exec(context.Background(), `INSERT INTO deployment_samples
		(deployment, cluster, at, gpu_per_replica, gpu_count) VALUES ($1,'c1',$2,$3,$4)`,
		deployment, at, gpuPer, gpuPer)
	if err != nil {
		t.Fatalf("insert shape: %v", err)
	}
}

func addUsageAt(t *testing.T, s *CostStore, tenant, endpoint string, durationSeconds, amountMicro int64, at time.Time) {
	t.Helper()
	_, err := s.db.pool.Exec(context.Background(), `INSERT INTO usage_events
		(tenant_id, project_id, key_id, model, endpoint_id, amounts_micro, duration_ms, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		tenant, tenant+"/research", "k1", "demo/model", endpoint,
		amountMicro, durationSeconds*1000, at)
	if err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

func consumptionFor(t *testing.T, s *CostStore, p cost.Period, tenant string) int64 {
	t.Helper()
	uses, err := s.consumption(context.Background(), p, "tenant_id")
	if err != nil {
		t.Fatalf("consumption: %v", err)
	}
	for _, u := range uses {
		if u.Key == tenant {
			return u.GPUSeconds
		}
	}
	return 0
}
