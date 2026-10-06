package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// A vendor charge and a pool share are different money, and the store is where
// they meet. These tests pin the boundary: which queries see a provider row, and
// which must not.

func addVendorUsage(t *testing.T, s *CostStore, p cost.Period,
	tenant, project, provider, endpoint string, amountMicro int64, at time.Time) {
	t.Helper()
	_, err := s.db.pool.Exec(context.Background(), `INSERT INTO usage_events
		(tenant_id, project_id, key_id, model, endpoint_id, provider,
		 amounts_micro, duration_ms, occurred_at)
		VALUES ($1,$2,'k1','gpt-4o',$3,$4,$5,4000,$6)`,
		tenant, project, endpoint, provider, amountMicro, at)
	if err != nil {
		t.Fatalf("insert vendor usage: %v", err)
	}
}

// The pool divides GPUs. A vendor request used none, so counting its wall clock
// would hand a tenant pool credit for somebody else's machine — and the report
// would still look plausible, because the numbers would be in range.
func TestAVendorRequestDoesNotOccupyThePool(t *testing.T) {
	s := newCostStore(t)
	if err := s.PutRate(context.Background(), cost.Rate{Cluster: "c1", GPUHourMicro: 1_000_000}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	p := period(t)
	fillMonth(t, s, 1, 1)

	// Two hours from the fleet and one from a vendor, inside the same month.
	//
	// The vendor row deliberately reuses the deployment's endpoint id, because
	// that is the case the provider filter exists for: the sweep joins usage to
	// deployment samples on endpoint id, so a vendor route sharing an id with a
	// deployment would otherwise be swept as though it had occupied that GPU.
	// With distinct ids the join already excludes it and the test would pass for
	// the wrong reason.
	addUsageAt(t, s, "acme", "fleet/llama", 7200, 1000, p.Start.Add(time.Hour))
	addVendorUsage(t, s, p, "acme", "acme/research", "openai", "fleet/llama",
		5_000_000, p.Start.Add(2*time.Hour))

	rep, err := s.ClosePeriod(context.Background(), p, cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, d := range rep.Deployments {
		if d.Name != "llama" {
			continue
		}
		// One replica of one GPU for the whole month, of which two hours was
		// busy. A three-hour busy figure would mean the vendor's request leaked
		// into the sweep.
		if want := int64(2 * 3600); d.Used > want {
			t.Fatalf("llama used %d GPU-seconds, want at most %d — a vendor request was counted as fleet usage",
				d.Used, want)
		}
	}
}

// A tenant that only ever called a vendor still has to appear on the report,
// with the money they are owed charged to them and no share of the operator's
// hardware invented for it.
func TestAVendorOnlyTenantIsBilledForWhatTheVendorCharged(t *testing.T) {
	s := newCostStore(t)
	if err := s.PutRate(context.Background(), cost.Rate{Cluster: "c1", GPUHourMicro: 1_000_000}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	p := period(t)
	fillMonth(t, s, 1, 1)
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 1000, p.Start.Add(time.Hour))
	addVendorUsage(t, s, p, "globex", "globex/chat", "openai", "openai/gpt-4o",
		7_000_000, p.Start.Add(2*time.Hour))

	rep, err := s.ClosePeriod(context.Background(), p, cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	if rep.Direct != 7_000_000 {
		t.Errorf("direct = %d, want 7000000", rep.Direct)
	}
	if len(rep.Providers) != 1 || rep.Providers[0].Provider != "openai" {
		t.Fatalf("providers = %+v, want one openai row", rep.Providers)
	}
	if rep.Providers[0].Requests != 1 {
		t.Errorf("requests = %d, want 1 — the count is what reconciles against the vendor statement",
			rep.Providers[0].Requests)
	}
	// Both cost centres key on the bare tenant, not on tenant/project: the pool
	// allocates by tenant because that is the billing relationship, and the
	// vendor's rows join to the same key so a scope has one row rather than two
	// that have to be added up by hand.
	row := allocationFor(rep, "globex")
	if row == nil {
		t.Fatalf("the vendor-only scope is missing from the report: %+v", rep.Tenants)
	}
	if row.Direct != 7_000_000 {
		t.Errorf("direct = %d, want 7000000", row.Direct)
	}
	if row.Amount != 0 || row.Share != 0 || row.GPUSeconds != 0 {
		t.Errorf("the vendor-only scope acquired a pool share: amount=%d share=%d gpuSeconds=%d",
			row.Amount, row.Share, row.GPUSeconds)
	}
	// And the pool must still divide in full: the vendor's presence must not
	// change what the operator's own GPUs cost their tenants.
	if rep.Allocated != rep.Pool {
		t.Errorf("allocated %d, want the whole pool %d", rep.Allocated, rep.Pool)
	}
}

// A closed period is an invoice. Reading it back must reproduce the direct
// figures, or the stored invoice silently loses the vendor's charges while the
// summary still shows a pool that was divided correctly.
func TestAClosedPeriodReadsBackItsVendorCharges(t *testing.T) {
	s := newCostStore(t)
	if err := s.PutRate(context.Background(), cost.Rate{Cluster: "c1", GPUHourMicro: 1_000_000}); err != nil {
		t.Fatalf("put rate: %v", err)
	}
	p := period(t)
	fillMonth(t, s, 1, 1)
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 1000, p.Start.Add(time.Hour))
	addVendorUsage(t, s, p, "acme", "acme/research", "openai", "openai/gpt-4o",
		4_000_000, p.Start.Add(2*time.Hour))
	addVendorUsage(t, s, p, "acme", "acme/research", "anthropic", "anthropic/claude",
		2_000_000, p.Start.Add(3*time.Hour))

	closed, err := s.ClosePeriod(context.Background(), p, cost.DefaultMinCoverage)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	read, err := s.GetPeriod(context.Background(), p.String())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if read.Direct != closed.Direct {
		t.Errorf("direct read back as %d, stored as %d", read.Direct, closed.Direct)
	}
	if len(read.Providers) != 2 {
		t.Fatalf("providers read back = %+v, want 2", read.Providers)
	}
	if read.Providers[0].Provider != "anthropic" || read.Providers[1].Provider != "openai" {
		t.Errorf("provider order = %q,%q — a stored invoice read twice must compare equal",
			read.Providers[0].Provider, read.Providers[1].Provider)
	}
	// Sorted by name, so the order is a property of the data rather than of the
	// insertion order; assert on the amounts by looking them up.
	byProvider := map[string]cost.ProviderSpend{}
	for _, row := range read.Providers {
		byProvider[row.Provider] = row
	}
	if byProvider["openai"].Amount != 4_000_000 || byProvider["anthropic"].Amount != 2_000_000 {
		t.Errorf("provider amounts = %+v", byProvider)
	}
	if got := allocationFor(read, "acme"); got == nil || got.Direct != 6_000_000 {
		t.Errorf("the scope's direct charge read back as %+v, want 6000000", got)
	}
}

// The open-period report has to split the same way, or the running figure and
// the eventual invoice would disagree about the same month.
func TestTheOpenSpendReportSplitsTheTwoCentres(t *testing.T) {
	s, p := newCostStore(t), period(t)
	addUsageAt(t, s, "acme", "fleet/llama", 3600, 1000, p.Start.Add(time.Hour))
	addVendorUsage(t, s, p, "acme", "acme/research", "openai", "openai/gpt-4o",
		3_000_000, p.Start.Add(2*time.Hour))

	scopes, err := s.db.SpendByScope(context.Background(),
		Window{From: p.Start, To: p.End})
	if err != nil {
		t.Fatalf("spend by scope: %v", err)
	}
	if len(scopes) != 1 {
		t.Fatalf("scopes = %+v, want one", scopes)
	}
	if scopes[0].UnitsMicro != 3_001_000 {
		t.Errorf("units = %d, want 3001000", scopes[0].UnitsMicro)
	}
	if scopes[0].DirectMicro != 3_000_000 {
		t.Errorf("direct = %d, want 3000000 — the split is what the console renders as two figures",
			scopes[0].DirectMicro)
	}
	providers, err := s.db.SpendByProvider(context.Background(),
		Window{From: p.Start, To: p.End})
	if err != nil {
		t.Fatalf("spend by provider: %v", err)
	}
	if len(providers) != 1 || providers[0].Provider != "openai" ||
		providers[0].Amount != 3_000_000 || providers[0].Requests != 1 {
		t.Errorf("providers = %+v", providers)
	}
	// The fleet is not a vendor and must never appear in that list, or the
	// console would offer to add a pool share to a vendor invoice.
	for _, row := range providers {
		if row.Provider == billing.NormalizeProvider("") {
			t.Error("the fleet appeared in the provider list")
		}
	}
}
