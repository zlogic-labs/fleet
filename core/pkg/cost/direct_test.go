package cost

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Direct spend is a separate column from the pool share, and these are the
// properties that keep it separate. Each test names the arithmetic that breaks
// if the two are merged: a pool share is a fraction of fixed capacity, a vendor
// charge is money per token, and the arithmetic downstream assumes they never
// meet.

func TestAVendorChargeLandsOnItsOwnRowWhenTheKeyUsedNoGPUs(t *testing.T) {
	rows := withDirect(nil, map[string]billing.Amount{
		"acme/bought-everything": 5000,
	})

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 — the scope exists whether or not it used a GPU", len(rows))
	}
	if rows[0].Key != "acme/bought-everything" {
		t.Errorf("key = %q", rows[0].Key)
	}
	if rows[0].Direct != 5000 {
		t.Errorf("direct = %d, want 5000", rows[0].Direct)
	}
	if rows[0].Amount != 0 || rows[0].GPUSeconds != 0 || rows[0].Share != 0 {
		t.Errorf("a direct-only scope must not acquire a pool share: amount=%d gpuSeconds=%d share=%d",
			rows[0].Amount, rows[0].GPUSeconds, rows[0].Share)
	}
}

// Merging the two is the mistake this file exists to prevent, so assert on the
// pool total as well as the row: a reader of Allocated must not be able to see
// vendor money in it.
func TestDirectSpendDoesNotTouchThePoolTotal(t *testing.T) {
	in := fullMonth()
	in.Rates = []Rate{{Cluster: "c1", GPUHourMicro: 1_000_000, Currency: "USD"}}
	in.Spans = map[string][]Span{"llama": held("acme/research", januaryHours/4)}
	in.Direct = map[string]billing.Amount{"globex/only-vendor": 9_000_000}
	in.Providers = map[string]ProviderCharge{
		"openai": {Amount: 9_000_000, Requests: 3},
	}

	rep, err := Close(in)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Direct != 9_000_000 {
		t.Errorf("direct = %d, want 9000000", rep.Direct)
	}
	// Every micro-unit of the pool is still apportioned, so a merge that folded
	// the vendor bill into Allocated would push it over. Exact equality rather
	// than a comparison, because that is the property: the pool is divided in
	// full and the vendor's money is not part of it.
	if rep.Allocated != rep.Pool {
		t.Errorf("allocated %d, want the whole pool %d — a vendor bill must not change how the pool divides",
			rep.Allocated, rep.Pool)
	}
	byKey := map[string]Tenant{}
	for _, row := range rep.Tenants {
		byKey[row.Key] = row
	}
	if got := byKey["acme/research"]; got.Direct != 0 {
		t.Errorf("the pool tenant has direct = %d; it used no vendor", got.Direct)
	}
	if got, ok := byKey["globex/only-vendor"]; !ok {
		t.Error("the vendor-only scope has no row at all; it would read as having spent nothing")
	} else if got.Share != 0 || got.GPUSeconds != 0 {
		t.Errorf("the vendor-only scope acquired pool usage: share=%d gpuSeconds=%d", got.Share, got.GPUSeconds)
	}
	// Total is defined as their sum, so this is the assertion that it is the
	// only place the addition happens.
	if rep.Total() != rep.Allocated+rep.Direct {
		t.Errorf("Total = %d, want %d", rep.Total(), rep.Allocated+rep.Direct)
	}
	// The pool must be exactly what the capacity cost, with no vendor rows in it.
	wantPool := billing.Amount(31 * 24 * 8 * 1_000_000)
	if rep.Pool != wantPool {
		t.Errorf("pool = %d, want %d — the pool is capacity only", rep.Pool, wantPool)
	}
}

// The provider rows are the figures an operator checks against a vendor's own
// statement. A report that listed one vendor twice would overstate the charge
// by exactly the amount the duplication hid.
func TestProvidersAreListedOnceEachAndSortedByName(t *testing.T) {
	rows := providerSpend(map[string]ProviderCharge{
		"openai":    {Amount: 300, Requests: 1},
		"anthropic": {Amount: 200, Requests: 2},
		"":          {Amount: 9_999, Requests: 9},
	})

	if len(rows) != 2 {
		t.Fatalf("providers = %d, want 2 — the fleet is not a vendor", len(rows))
	}
	if rows[0].Provider != "anthropic" || rows[1].Provider != "openai" {
		t.Errorf("order = %q, %q; want sorted by name so two closes of the same data are identical",
			rows[0].Provider, rows[1].Provider)
	}
	if got := totalDirect(rows); got != 500 {
		t.Errorf("total direct = %d, want 500", got)
	}
}

// A zero is not a charge. Reporting it would put a vendor row in the invoice
// for a month in which it was not used, which is the kind of line item that
// takes an afternoon to argue away.
func TestAVendorThatWasNotUsedIsNotListed(t *testing.T) {
	rows := providerSpend(map[string]ProviderCharge{"openai": {Amount: 0, Requests: 12}})
	if len(rows) != 0 {
		t.Errorf("providers = %+v, want none", rows)
	}
	if got := totalDirect(rows); got != 0 {
		t.Errorf("total = %d, want 0", got)
	}
}

// Matching by index instead of by key would attach one tenant's vendor bill to
// whichever tenant happened to land on the same position after allocate sorted
// its output.
func TestDirectIsMatchedByKeyNotByPosition(t *testing.T) {
	rows := []Tenant{
		{Key: "acme/a", Amount: 100, GPUSeconds: 10},
		{Key: "globex/b", Amount: 200, GPUSeconds: 10},
		{Key: "initech/c", Amount: 300, GPUSeconds: 10},
	}
	out := withDirect(rows, map[string]billing.Amount{
		"acme/a":      11,
		"initech/c":   33,
		"zeta/only":   55,
		"globex/none": 0,
	})

	byKey := map[string]billing.Amount{}
	for _, r := range out {
		byKey[r.Key] = r.Direct
	}
	if byKey["acme/a"] != 11 || byKey["initech/c"] != 33 || byKey["globex/b"] != 0 {
		t.Errorf("direct by key = %v; a charge landed on the wrong tenant", byKey)
	}
	if byKey["zeta/only"] != 55 {
		t.Errorf("the direct-only scope is missing or short: %v", byKey)
	}
	if _, ok := byKey["globex/none"]; ok {
		t.Error("a zero charge created a row for a scope that used neither centre")
	}
}
