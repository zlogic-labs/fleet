package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The spend budget, end to end through a real gateway.
//
// The store's own tests prove the reservation arithmetic; these prove the
// request path honours it — that a refused budget never reaches an engine, and
// that the counter and the ledger are written from the same number.
// A tenant that has spent its budget is refused with 402, not 429. A client
// that retries a 429 is behaving correctly; one that retries this is spending
// money proving it cannot succeed, and the retry is what runs the GPU.
func TestASpentBudgetIsRefusedWithPaymentRequired(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	// One unit of budget against a request that reserves far more.
	h, key := buildBudgeted(t, db, 1, up.URL, "broke")

	w := post(h, key, req)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", w.Code, w.Body)
	}
	if after := w.Header().Get("Retry-After"); after == "" {
		t.Error("no Retry-After; a client cannot tell when to come back")
	}
	if !strings.Contains(w.Body.String(), "budget_exhausted") {
		t.Errorf("body = %s, want a budget_exhausted code a client can branch on", w.Body)
	}
	// The engine was never called: a refused budget must not have run a GPU.
	if got := calls.Load(); got != 0 {
		t.Errorf("the engine was called %d times for a refused request", got)
	}
	if rows := countRows(t, db); rows != 0 {
		t.Errorf("%d ledger rows for a refused request", rows)
	}
}

// The budget and the ledger must agree. They are written from the same figure,
// and a disagreement between them is what reconciliation would eventually find
// — so the test asserts it directly rather than trusting the code path.
func TestTheBudgetAndTheLedgerAgree(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h, key := buildBudgeted(t, db, 1000, up.URL, "agree")

	for i := 0; i < 3; i++ {
		if w := post(h, key, req); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
	}

	var ledgerMicro int64
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amounts_micro),0) FROM usage_events`).Scan(&ledgerMicro); err != nil {
		t.Fatalf("sum the ledger: %v", err)
	}
	if ledgerMicro == 0 {
		t.Fatal("the ledger recorded nothing; the comparison would be vacuous")
	}

	// One counter row per level, each carrying the whole charge — the envelope
	// and the partition are two views of the same spending, not two spendings.
	// Summing the rows would double the ledger and prove nothing, so each is
	// compared on its own.
	rows, err := db.Pool().Query(context.Background(),
		`SELECT scope, spent_micro FROM spend_counters ORDER BY scope`)
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}
	defer rows.Close()
	var seen int
	for rows.Next() {
		var (
			scope string
			spent int64
		)
		if err := rows.Scan(&scope, &spent); err != nil {
			t.Fatalf("scan the counters: %v", err)
		}
		if spent != ledgerMicro {
			t.Errorf("counter %s says %d micro, the ledger says %d", scope, spent, ledgerMicro)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the counters: %v", err)
	}
	if seen != 2 {
		t.Errorf("%d counter rows, want 2 (the tenant envelope and the project partition)", seen)
	}
}
