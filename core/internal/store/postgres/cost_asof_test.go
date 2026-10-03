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

	seconds := sweptFor(t, s, p, "acme")
	if want := int64(3600 * 2); seconds != want {
		t.Fatalf("priced %d GPU-seconds, want %d (the shape at request time)", seconds, want)
	}
}

func TestARequestBeforeAnySampleCostsNothingRatherThanGuessing(t *testing.T) {
	s := newCostStore(t)
	p := period(t)
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 500_000, p.Start.Add(time.Hour))
	shape(t, s, "fleet/llama", 2, p.Start.Add(4*time.Hour))

	if got := sweptFor(t, s, p, "acme"); got != 0 {
		t.Fatalf("priced %d GPU-seconds against a shape Fleet had not seen yet", got)
	}
}

func TestConcurrentRequestsDoNotMultiplyWhatTheTenantIsCharged(t *testing.T) {
	// Ten requests occupying the same single GPU for the same minute is one
	// minute of that GPU. Summing wall clock bills ten, and the error scales
	// with exactly the concurrency Fleet exists to absorb.
	s := newCostStore(t)
	p := period(t)
	shape(t, s, "fleet/llama", 1, p.Start)
	for range 10 {
		addUsageAt(t, s, "acme", "fleet/llama", 60, 0, p.Start.Add(time.Minute))
	}

	if got, want := sweptFor(t, s, p, "acme"), int64(60); got != want {
		t.Fatalf("billed %d GPU-seconds for ten concurrent minutes on one GPU, want %d", got, want)
	}
}

func TestAnOverlappingDeploymentNeverCostsMoreThanItHeld(t *testing.T) {
	// The clamp that used to hide this is gone, so the bound has to hold at the
	// store boundary too, not just in the sweep's own tests.
	s := newCostStore(t)
	p := period(t)
	shape(t, s, "fleet/llama", 2, p.Start)
	for i := range 8 {
		addUsageAt(t, s, "acme", "fleet/llama", 600, 0, p.Start.Add(time.Duration(i+1)*time.Minute))
	}

	rep, err := s.ClosePeriod(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rep.Deployments) != 1 {
		t.Fatalf("got %d deployments", len(rep.Deployments))
	}
	d := rep.Deployments[0]
	if d.Idle < 0 || d.Used > d.Reserved {
		t.Fatalf("reserved %d used %d idle %d", d.Reserved, d.Used, d.Idle)
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

// sweptFor runs what Close runs, per tenant, so a test can assert on the
// number that ends up on an invoice rather than on the rows behind it.
func sweptFor(t *testing.T, s *CostStore, p cost.Period, tenant string) int64 {
	t.Helper()
	in := cost.Input{Period: p}
	if err := s.gather(context.Background(), p, &in); err != nil {
		t.Fatalf("gather: %v", err)
	}
	var seconds int64
	for deployment, group := range in.Spans {
		seconds += cost.Sweep(in.Reserved[deployment], group, p.Start, p.End).ByKey[tenant]
	}
	return seconds
}
