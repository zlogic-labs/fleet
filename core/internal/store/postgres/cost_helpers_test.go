package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

type billingAmount = billing.Amount

// billingUnits is one whole currency unit in micro, so the expectations below
// read as dollars rather than as eleven-digit integers.
const billingUnits = billing.MicroPerUnit

// costTables is everything a close reads or writes.
var costTables = []string{
	"cost_adjustments", "cost_allocations", "cost_providers", "cost_periods", "cost_rates",
	"deployment_samples", "capacity_samples", "usage_events",
}

func newCostStore(t *testing.T) *CostStore {
	t.Helper()
	db := testDB(t)
	truncate(t, db, costTables...)
	return NewCostStore(db)
}

func periodStart() time.Time {
	return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
}

// period is the month the fixtures build a month of samples for.
func period(t *testing.T) cost.Period {
	t.Helper()
	p, err := cost.ParsePeriod("2026-03")
	if err != nil {
		t.Fatalf("parse period: %v", err)
	}
	return p
}

func node(name string, gpus int) inventory.Node {
	return inventory.Node{Name: name, GPU: inventory.GPU{Count: gpus}}
}

func report(gpus, replicas int) inventory.Report {
	return inventory.Report{
		Cluster: inventory.Cluster{
			Name:       "c1",
			ReportedAt: periodStart(),
			Nodes:      []inventory.Node{node("n1", gpus)},
		},
		Deployments: []inventory.Deployment{{
			Name: "llama", Namespace: "fleet", GPUPer: 1, Desired: replicas,
		}},
	}
}

// fillMonth writes capacity samples across the whole period, so a close has
// something to integrate.
func fillMonth(t *testing.T, s *CostStore, gpus, replicas int) {
	t.Helper()
	fillPeriod(t, s, period(t), gpus, replicas)
}

// fillPeriod writes capacity across one period, bounded by that period's own
// length.
//
// Bounded rather than a fixed 31 days because a fixed month writes past the end
// of a short one: filling June for 31 days lands samples in July, and since a
// deployment sample is keyed on (deployment, at) the second fill collides with
// the first. That collision is only visible in a test that closes more than one
// month — and closing more than one month is the normal case.
func fillPeriod(t *testing.T, s *CostStore, p cost.Period, gpus, replicas int) {
	t.Helper()
	ctx := context.Background()
	// Six-hour spacing is enough: Integrate interpolates between samples, and a
	// constant value integrates to the same total at any spacing.
	for at := p.Start; at.Before(p.End); at = at.Add(6 * time.Hour) {
		r := report(gpus, replicas)
		r.Cluster.ReportedAt = at
		if err := s.RecordCapacity(ctx, r); err != nil {
			t.Fatalf("record capacity at %s: %v", at, err)
		}
	}
}

func countRows(t *testing.T, s *CostStore, table string) int {
	t.Helper()
	var n int
	if err := s.db.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func onlySample(t *testing.T, s *CostStore, table string) int64 {
	t.Helper()
	var n int64
	if err := s.db.pool.QueryRow(context.Background(),
		`SELECT gpu_count FROM `+table+` LIMIT 1`).Scan(&n); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return n
}

// addUsage writes one settled request as the gateway would.
func addUsage(t *testing.T, s *CostStore, tenant, endpoint string, durationSeconds, amountMicro int64) {
	t.Helper()
	addUsageAt(t, s, tenant, endpoint, durationSeconds, amountMicro, periodStart().Add(2*time.Hour))
}

func errorAs(err error, target **cost.ErrIncomplete) bool { return errors.As(err, target) }
