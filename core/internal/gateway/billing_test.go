package gateway

import (
	"sync/atomic"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// Settlement, against a real database and a real price.
//
// The store's own tests prove the ledger stores what it is given and the
// pricer computes what it should. Neither can prove the gateway hands them the
// right things — that the record carries the tenant the request authenticated
// as, the model the endpoint actually served, and the engine's own token counts
// rather than the ones the client asked for. That is what this is for.

// pricedModel is the model name the stub engine serves, which is what the
// endpoint resolves to. The gateway records the resolved model, never the
// string the client sent.
const pricedModel = "demo"

func TestSettlementWritesTheLedgerRow(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h, key := buildBilling(t, db, up.URL, "wired")

	// The stub engine reports prompt 5, completion 2, total 7.
	if w := post(h, key, req); w.Code != 200 {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}

	rec := onlyRecord(t, db)
	// The project is recorded as its id, not its bare name. Two tenants may
	// each have a "research" project, and the ledger has to be able to tell
	// them apart — it is also the column the control plane counts when it
	// refuses to delete a project that has already been billed.
	if rec.Tenant != "acme" || rec.Project != "acme/research" {
		t.Errorf("attributed to tenant %q project %q, want acme / acme/research",
			rec.Tenant, rec.Project)
	}
	// The key id is the stored row's id, which is the full tenant/project/label
	// path. It is recorded so one key can be traced without joining anything.
	if rec.KeyID != "acme/research/wired" {
		t.Errorf("key id = %q, want acme/research/wired", rec.KeyID)
	}
	// The resolved model, not the client's string. A client asking for a
	// cheap alias must not be billed at the cheap model.
	if rec.Model != pricedModel {
		t.Errorf("model = %q, want the resolved %q", rec.Model, pricedModel)
	}
	// The engine's own numbers, not the reservation's upper bound. The client
	// asked for 4096 and the engine used 7; charging 4096 is the bug P6 exists
	// to prevent.
	if rec.Usage.TotalTokens != 7 {
		t.Errorf("total tokens = %d, want the engine's 7", rec.Usage.TotalTokens)
	}
	if !rec.UsageKnown {
		t.Error("usage_known is false although the engine reported usage")
	}
	if rec.Amount <= 0 {
		t.Errorf("amount = %d, want a positive charge — the request was priced at nothing", rec.Amount)
	}
	if rec.PriceBook == "" {
		t.Error("the record quotes no price book; the audit trail does not say what it was charged at")
	}
}

// A model with no price is still recorded, at zero, and the tokens are not lost.
// A model serving traffic with no price is a customer being given a GPU for
// free, and the row that shows the tokens spent is how anybody finds out.
func TestAnUnpricedModelIsStillRecorded(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h, key := buildBilling(t, db, up.URL, "unpriced")

	if w := post(h, key, req); w.Code != 200 {
		t.Fatalf("an unpriced model broke the request: %d %s", w.Code, w.Body)
	}

	rec := onlyRecord(t, db)
	if rec.Usage.TotalTokens != 7 {
		t.Errorf("total tokens = %d, want 7 — an unpriced request still spent tokens", rec.Usage.TotalTokens)
	}
	if rec.Amount != 0 {
		t.Errorf("amount = %d, want 0 for a model with no price", rec.Amount)
	}
	if rec.PriceBook != "" {
		t.Errorf("price book = %q, want empty for a model with no price", rec.PriceBook)
	}
}

// An engine that reports no usage is charged at max_tokens and marked, so
// reconciliation can find it later rather than it being invisible in an
// aggregate. This is the P6 fallback.
func TestAnEngineWithoutUsageIsRecordedAsAnEstimate(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := silentEngine(t, &calls)
	h, key := buildBilling(t, db, up.URL, "silent")

	if w := post(h, key, req); w.Code != 200 {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}

	rec := onlyRecord(t, db)
	if rec.UsageKnown {
		t.Error("usage_known is true although the engine reported no usage")
	}
	if rec.Usage.TotalTokens == 0 {
		t.Error("an estimate recorded zero tokens; the spend would be invisible")
	}
	if rec.Amount <= 0 {
		t.Errorf("amount = %d — an estimate is still charged at max_tokens", rec.Amount)
	}
}

// The reservation is an upper bound, not the bill. A client that omits
// max_tokens reserves DefaultMaxTokens and gets seven tokens back; settling the
// reservation instead of the result would let two such requests exhaust a
// tenant's minute that can hold a hundred of them.
//
// The limit is chosen so the two behaviours differ: the reservation alone
// exceeds it, so a gateway that fails to refund refuses the second request.
func TestARequestIsRefundedWhatItDidNotSpend(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h, key := buildBillingTokenLimit(t, db, 2000, up.URL, "refund")

	for i := 0; i < 2; i++ {
		w := post(h, key, req)
		if w.Code != 200 {
			t.Fatalf("request %d: %d %s — the reservation was not refunded",
				i+1, w.Code, w.Body)
		}
	}
}

// A gateway with no database records nothing and still serves. The nil
// recorder must mean "keep no ledger", not "fail every request".
func TestNoDatabaseMeansNoLedgerAndStillServes(t *testing.T) {
	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true, Keys: []string{"acme/research/k"}},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: pricedModel, BaseURL: up.URL}},
	}
	h, _, err := Build(cfg, nil, entitlement.Community(), discardLogger(), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if w := post(h, "acme/research/k", req); w.Code != 200 {
		t.Errorf("a gateway with no database refused a request: %d %s", w.Code, w.Body)
	}
}
