package postgres

import (
	"context"
	"testing"
	"time"
)

func TestCapacitySamplesAreWrittenOncePerHeartbeat(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	// Nine reports, four minutes apart in total: under the heartbeat, so one row.
	for i := 0; i < 9; i++ {
		r := report(8, 2)
		r.Cluster.ReportedAt = periodStart().Add(time.Duration(i) * 30 * time.Second)
		if err := s.RecordCapacity(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if n := countRows(t, s, "capacity_samples"); n != 1 {
		t.Fatalf("%d capacity samples from nine unchanged reports inside the heartbeat, want 1", n)
	}
}

func TestACapacityChangeIsRecordedImmediately(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	base := report(8, 2)
	base.Cluster.ReportedAt = periodStart()
	if err := s.RecordCapacity(ctx, base); err != nil {
		t.Fatalf("record: %v", err)
	}
	// One second later the fleet doubles. Waiting for the heartbeat would bill
	// the old capacity for the next five minutes.
	grown := report(16, 2)
	grown.Cluster.ReportedAt = periodStart().Add(time.Second)
	if err := s.RecordCapacity(ctx, grown); err != nil {
		t.Fatalf("record: %v", err)
	}
	if n := countRows(t, s, "capacity_samples"); n != 2 {
		t.Fatalf("%d samples after a doubling, want 2", n)
	}
}

func TestNotReadyGPUsStillCountTowardsThePool(t *testing.T) {
	// A GPU that is cordoned and failing is still billed by the cloud provider.
	s := newCostStore(t)
	r := report(8, 1)
	r.Cluster.Nodes[0].Ready = false
	r.Cluster.ReportedAt = periodStart()
	if err := s.RecordCapacity(context.Background(), r); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := onlySample(t, s, "capacity_samples"); got != 8 {
		t.Fatalf("pool counts %d GPUs, want 8", got)
	}
}

func TestDeploymentShapeIsKeptBesideTheCount(t *testing.T) {
	// Total unchanged, shape changed: without both rows the whole month is
	// priced against whichever shape is current when the invoice is drawn.
	s := newCostStore(t)
	ctx := context.Background()
	r := report(8, 4)
	r.Cluster.ReportedAt = periodStart()
	r.Deployments[0].GPUPer = 1
	if err := s.RecordCapacity(ctx, r); err != nil {
		t.Fatalf("record: %v", err)
	}
	r.Deployments[0].GPUPer = 4
	r.Deployments[0].Desired = 1
	r.Cluster.ReportedAt = periodStart().Add(time.Hour)
	if err := s.RecordCapacity(ctx, r); err != nil {
		t.Fatalf("record: %v", err)
	}
	var shapes int
	err := s.db.pool.QueryRow(ctx,
		`SELECT count(*) FROM deployment_samples WHERE gpu_per_replica IN (1, 4)`).Scan(&shapes)
	if err != nil {
		t.Fatalf("count shapes: %v", err)
	}
	if shapes != 2 {
		t.Fatalf("got %d deployment samples, want one per shape", shapes)
	}
}
