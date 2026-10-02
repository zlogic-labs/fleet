package postgres

import (
	"context"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

func TestClosePricesThePoolAndAllocatesIt(t *testing.T) {
	s := newCostStore(t)
	fillMonth(t, s, 8, 2)
	if err := s.PutRate(context.Background(),
		cost.Rate{Cluster: "c1", GPUHourMicro: 3_000_000, Currency: "USD"}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	addUsage(t, s, "acme", "fleet/llama", 4*3600, 1_000_000)

	rep, err := s.ClosePeriod(context.Background(), period(t), cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !rep.Priced || rep.Currency != "USD" {
		t.Fatalf("report is not priced: %+v", rep)
	}
	if rep.PoolSeconds != 8*31*24*3600 {
		t.Fatalf("pool seconds %d, want %d", rep.PoolSeconds, 8*31*24*3600)
	}
	if rep.Pool != 8*31*24*3*billingUnits {
		t.Fatalf("pool %s, want %d units", rep.Pool, 8*31*24*3)
	}
	if len(rep.Tenants) != 1 || rep.Tenants[0].Key != "acme" {
		t.Fatalf("tenants %+v", rep.Tenants)
	}
	if rep.Tenants[0].UsageMicro != 1_000_000 {
		t.Fatalf("ledger spend %d, want 1000000", rep.Tenants[0].UsageMicro)
	}
}

func TestCloseRefusesAPeriodItDidNotWatch(t *testing.T) {
	s := newCostStore(t)
	r := report(8, 1)
	r.Cluster.ReportedAt = periodStart()
	if err := s.RecordCapacity(context.Background(), r); err != nil {
		t.Fatalf("record: %v", err)
	}
	_, err := s.ClosePeriod(context.Background(), period(t), cost.DefaultMinCoverage)
	var incomplete *cost.ErrIncomplete
	if !errorAs(err, &incomplete) {
		t.Fatalf("got %v, want ErrIncomplete", err)
	}
}

func TestClosingTwiceIsAConflict(t *testing.T) {
	// An invoice that can change after it was sent is not an invoice.
	s := newCostStore(t)
	fillMonth(t, s, 4, 1)
	ctx := context.Background()
	if _, err := s.ClosePeriod(ctx, period(t), cost.DefaultMinCoverage); err != nil {
		t.Fatalf("first close: %v", err)
	}
	_, err := s.ClosePeriod(ctx, period(t), cost.DefaultMinCoverage)
	if !Conflict(err) {
		t.Fatalf("got %v, want a conflict", err)
	}
}

func TestAClosedPeriodReadsBackIdentically(t *testing.T) {
	s := newCostStore(t)
	fillMonth(t, s, 8, 2)
	ctx := context.Background()
	if err := s.PutRate(ctx, cost.Rate{Cluster: "c1", GPUHourMicro: 2_500_000}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	addUsage(t, s, "acme", "fleet/llama", 3600, 500_000)
	addUsage(t, s, "zeta", "fleet/llama", 3600, 500_000)

	closed, err := s.ClosePeriod(ctx, period(t), cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	read, err := s.GetPeriod(ctx, "2026-03")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Pool != closed.Pool || read.Idle != closed.Idle || read.IdlePct != closed.IdlePct {
		t.Fatalf("read back %+v, closed %+v", read, closed)
	}
	var sum billingAmount
	for _, tr := range read.Tenants {
		sum += tr.Amount
	}
	if sum != closed.Allocated {
		t.Fatalf("allocations read back as %s, closed report says %s", sum, closed.Allocated)
	}
}

func TestGetPeriodOnSomethingNeverClosed(t *testing.T) {
	s := newCostStore(t)
	if _, err := s.GetPeriod(context.Background(), "2026-03"); !NotFound(err) {
		t.Fatalf("got %v, want not found", err)
	}
}

func TestRatesRejectNonsense(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	if err := s.PutRate(ctx, cost.Rate{}); !Invalid(err) {
		t.Fatalf("an empty cluster gave %v", err)
	}
	if err := s.PutRate(ctx, cost.Rate{Cluster: "c1", GPUHourMicro: -1}); !Invalid(err) {
		t.Fatalf("a negative rate gave %v", err)
	}
	if err := s.PutRate(ctx, cost.Rate{Cluster: "c1"}); err != nil {
		t.Fatalf("a zero rate is legal — a free on-prem GPU — got %v", err)
	}
}

func TestUpsertingARateReplacesIt(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	for _, micro := range []int64{1_000_000, 4_000_000} {
		if err := s.PutRate(ctx, cost.Rate{Cluster: "c1", GPUHourMicro: micro}); err != nil {
			t.Fatalf("put rate: %v", err)
		}
	}
	rates, err := s.ListRates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rates) != 1 || rates[0].GPUHourMicro != 4_000_000 {
		t.Fatalf("rates %+v", rates)
	}
	if rates[0].Currency != "USD" {
		t.Fatalf("currency defaulted to %q", rates[0].Currency)
	}
}
