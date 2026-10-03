package postgres

import (
	"context"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// The period list is what the console's Cost page renders before anybody opens
// a report, so a field it leaves at zero is a warning the page shows against a
// closed invoice.
//
// The failure this pins down was subtle in the way these tend to be: the close
// gate had refused to accept the month until 90% of it was observed, so
// coverage was demonstrably high — and the list still reported 0%, because the
// query selected seven of the nine columns the type carries. Nothing crashed
// and nothing was obviously wrong; the page just claimed a fully observed
// month was entirely unobserved.

func TestThePeriodListCarriesTheFieldsThePageShows(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	fillMonth(t, s, 8, 3)
	if err := s.PutRate(ctx, cost.Rate{Cluster: "c1", GPUHourMicro: 2_000_000, Currency: "USD"}); err != nil {
		t.Fatalf("declare rate: %v", err)
	}
	closed, err := s.ClosePeriod(ctx, period(t), cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	periods, err := s.ListPeriods(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(periods) != 1 {
		t.Fatalf("listed %d periods, want 1", len(periods))
	}
	got := periods[0]

	// Every field the console reads off a list row, compared against what the
	// close produced for the same period. A summary that disagrees with the
	// report it summarises is worse than no summary.
	if got.CoveragePercent != closed.CoveragePercent {
		t.Errorf("coverage %d%% in the list, %d%% in the report",
			got.CoveragePercent, closed.CoveragePercent)
	}
	if got.CoveragePercent < 90 {
		t.Fatalf("fixture built a fully observed month, coverage came out %d%%", got.CoveragePercent)
	}
	if got.PoolSeconds != closed.PoolSeconds {
		t.Errorf("pool %d gpu-seconds in the list, %d in the report", got.PoolSeconds, closed.PoolSeconds)
	}
	if got.PoolSeconds <= 0 {
		t.Errorf("pool is %d gpu-seconds; a month of 8 GPUs cannot be zero", got.PoolSeconds)
	}
	if got.Pool != closed.Pool || got.Idle != closed.Idle || got.IdlePct != closed.IdlePct {
		t.Errorf("money differs: list pool=%d idle=%d idlePct=%d, report pool=%d idle=%d idlePct=%d",
			got.Pool, got.Idle, got.IdlePct, closed.Pool, closed.Idle, closed.IdlePct)
	}
	if got.Currency != "USD" || !got.Priced {
		t.Errorf("list says currency=%q priced=%v, want USD and true", got.Currency, got.Priced)
	}
}

func TestThePeriodListIsNewestFirst(t *testing.T) {
	s := newCostStore(t)
	ctx := context.Background()
	for _, p := range []string{"2026-05", "2026-07", "2026-06"} {
		per, err := cost.ParsePeriod(p)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		fillPeriod(t, s, per, 2, 1)
		if _, err := s.ClosePeriod(ctx, per, 0); err != nil {
			t.Fatalf("close %s: %v", p, err)
		}
	}
	periods, err := s.ListPeriods(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"2026-07", "2026-06", "2026-05"}
	for i, w := range want {
		if i >= len(periods) {
			t.Fatalf("only %d periods listed", len(periods))
		}
		if periods[i].Period != w {
			t.Errorf("position %d is %s, want %s", i, periods[i].Period, w)
		}
	}
}
