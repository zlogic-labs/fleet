package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Recomputing a closed period, and where the difference goes.

func nextMonth(t *testing.T) cost.Period {
	t.Helper()
	p, err := cost.ParsePeriod("2026-04")
	if err != nil {
		t.Fatalf("parse period: %v", err)
	}
	return p
}

// closeBoth declares a price and fills two months of capacity, so a close has
// something to integrate and a later month exists to carry a correction.
func closeBoth(t *testing.T) (*CostStore, cost.Period, cost.Period) {
	t.Helper()
	s := newCostStore(t)
	if err := s.PutRate(context.Background(), cost.Rate{Cluster: "c1", GPUHourMicro: 2_000_000}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	p, q := period(t), nextMonth(t)
	fillPeriod(t, s, p, 1, 1)
	fillPeriod(t, s, q, 1, 1)
	return s, p, q
}

// requestFor writes one hour of a tenant's traffic inside p.
func requestFor(t *testing.T, s *CostStore, p cost.Period, tenant string) {
	t.Helper()
	addUsageAt(t, s, tenant, "fleet/llama", 3600, 1000, p.Start.Add(2*time.Hour))
}

func allocationFor(rep cost.Report, scope string) *cost.Tenant {
	for i := range rep.Tenants {
		if rep.Tenants[i].Key == scope {
			return &rep.Tenants[i]
		}
	}
	return nil
}

func TestARecomputationIsAcceptedUntilAMonthAfterItIsClosed(t *testing.T) {
	s, p, _ := closeBoth(t)
	ctx := context.Background()
	requestFor(t, s, p, "acme")

	first, err := s.ClosePeriod(ctx, p, 0)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if first.Revision != 1 {
		t.Fatalf("first close is revision %d, want 1", first.Revision)
	}

	second, err := s.ClosePeriod(ctx, p, 0)
	if err != nil {
		t.Fatalf("reclose: %v", err)
	}
	if second.Revision != 2 {
		t.Fatalf("second close is revision %d, want 2", second.Revision)
	}
	if countRows(t, s, "cost_adjustments") != 0 {
		t.Fatal("a recomputation that changed nothing booked an adjustment")
	}
}

func TestAFrozenPeriodSaysWhichMonthClosedTheDoor(t *testing.T) {
	s, p, q := closeBoth(t)
	ctx := context.Background()
	requestFor(t, s, p, "acme")
	if _, err := s.ClosePeriod(ctx, p, 0); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.ClosePeriod(ctx, q, 0); err != nil {
		t.Fatalf("close the next month: %v", err)
	}

	_, err := s.ClosePeriod(ctx, p, 0)
	var frozen *ErrFrozen
	if !errors.As(err, &frozen) {
		t.Fatalf("got %v, want ErrFrozen", err)
	}
	if frozen.After != "2026-04" {
		t.Fatalf("frozen because of %q", frozen.After)
	}
}

func TestALateTenantMovesTheBillIntoTheFollowingMonth(t *testing.T) {
	// The case the whole mechanism exists for: a month's invoice was issued
	// before every row for it had been written. Here zeta's request turns up
	// afterwards, which halves what acme was billed and gives zeta the other
	// half. March's invoice does not move; April collects the difference.
	s, p, q := closeBoth(t)
	ctx := context.Background()
	requestFor(t, s, p, "acme")

	first, err := s.ClosePeriod(ctx, p, 0)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := allocationFor(first, "acme"); got == nil || got.Amount != first.Pool {
		t.Fatalf("acme %+v out of a pool of %d", got, first.Pool)
	}

	// The late arrival, inside March, written after March was closed.
	requestFor(t, s, p, "zeta")

	revised, err := s.ClosePeriod(ctx, p, 0)
	if err != nil {
		t.Fatalf("reclose: %v", err)
	}
	if allocationFor(revised, "zeta") == nil {
		t.Fatalf("the late tenant is not in the revision: %+v", revised.Tenants)
	}

	amended, err := s.GetPeriod(ctx, p.String())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(amended.Amended) != 2 {
		t.Fatalf("amended %+v, want one row per scope that moved", amended.Amended)
	}
	for _, a := range amended.Amended {
		if a.ForPeriod != "2026-04" {
			t.Fatalf("the difference is booked into %q", a.ForPeriod)
		}
	}
	// A redistribution between two tenants moves money but creates none, so
	// the two amounts cancel. Reporting only the net would say "nothing
	// happened", which is exactly what the operator needs to be told is false.
	if acme, zeta := amended.Amended[0], amended.Amended[1]; acme.Scope != "acme" ||
		acme.Amount >= 0 || zeta.Scope != "zeta" || zeta.Amount <= 0 {
		t.Fatalf("amendments %+v", amended.Amended)
	}
	if amended.Amended[0].Amount+amended.Amended[1].Amount != 0 {
		t.Fatalf("a redistribution created or destroyed money: %+v", amended.Amended)
	}

	// April collects the difference while carrying its own traffic, so it is an
	// ordinary invoice rather than an empty month with corrections bolted on.
	requestFor(t, s, q, "globex")

	next, err := s.ClosePeriod(ctx, q, 0)
	if err != nil {
		t.Fatalf("close the next month: %v", err)
	}
	if len(next.Adjustments) != 2 {
		t.Fatalf("the next month collected %d corrections, want 2: %+v", len(next.Adjustments), next.Adjustments)
	}
	// The two cancel: this correction moved a share between tenants and created
	// no money, so the invoice is still exactly the pool. A total that ignored
	// that would silently invent a charge; one that reported the gross would
	// say April collected twice what it did.
	if next.AdjustmentTotal != 0 {
		t.Fatalf("a redistribution changed the total by %s", next.AdjustmentTotal)
	}
	if next.Allocated != next.Pool {
		t.Fatalf("allocated %s against a pool of %s", next.Allocated, next.Pool)
	}
	if zeta := allocationFor(next, "zeta"); zeta == nil || zeta.Adjustment <= 0 {
		t.Fatalf("zeta owes a correction but the row says %+v", zeta)
	}
	if acme := allocationFor(next, "acme"); acme == nil || acme.Adjustment >= 0 {
		t.Fatalf("acme is refunded by the correction but the row says %+v", acme)
	}
	if globex := allocationFor(next, "globex"); globex == nil || globex.Adjustment != 0 {
		t.Fatalf("globex is caught up in a correction it was not part of: %+v", globex)
	}
}

func TestRevisingTwiceAccumulatesIntoOneMonth(t *testing.T) {
	s, p, q := closeBoth(t)
	ctx := context.Background()
	requestFor(t, s, p, "acme")
	if _, err := s.ClosePeriod(ctx, p, 0); err != nil {
		t.Fatalf("close: %v", err)
	}

	requestFor(t, s, p, "zeta")
	if _, err := s.ClosePeriod(ctx, p, 0); err != nil {
		t.Fatalf("reclose: %v", err)
	}
	requestFor(t, s, p, "initech")
	if _, err := s.ClosePeriod(ctx, p, 0); err != nil {
		t.Fatalf("third close: %v", err)
	}

	var n int
	err := s.db.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM cost_adjustments WHERE period = $1 AND for_period = $2`,
		q.String(), p.String()).Scan(&n)
	if err != nil {
		t.Fatalf("count adjustments: %v", err)
	}
	if n != 3 {
		t.Fatalf("%d adjustment rows, want one per tenant", n)
	}
}
