package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
)

// A gateway with a database and no authentication is the single-tenant private
// deployment: the operator owns the GPUs, issues no keys, and wants their own
// traffic metered. It answered every request 503 — a budget names a scope, a
// bare request names none, and the refusal read as a broken platform rather
// than as "there is nothing here to enforce against".
func TestAnonymousTrafficIsServedAndStillMetered(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h := buildAnonymousWired(t, db, up.URL)

	if w := post(h, "", req); w.Code != http.StatusOK {
		t.Fatalf("anonymous request against a database: %d, want 200 — %s",
			w.Code, w.Body.String())
	}
	assertMetered(t, db)
}

// The counterpart, so the fix above cannot be read as "anonymous traffic is
// free": a named tenant is still budgeted. This would also pass if the
// reservation were simply skipped for everyone.
//
// The refused case itself is TestASpentBudgetIsRefusedWithPaymentRequired.
// What is asserted here is only the difference between the two requests —
// same gateway shape, one tenant present and one absent.
func TestATenantIsStillBudgetedWhereAnonymousIsNot(t *testing.T) {
	db := wireDB(t)
	truncateWired(t, db)
	seedPrice(t, db)

	var calls atomic.Int64
	up := fakeEngine(t, &calls)
	h, key := buildBudgeted(t, db, 1, up.URL, "named")

	if w := post(h, key, req); w.Code != http.StatusPaymentRequired {
		t.Errorf("a named tenant past its budget: %d, want 402 — %s", w.Code, w.Body.String())
	}
}

func buildAnonymousWired(t *testing.T, db *sqlstore.DB, engineURL string) http.Handler {
	t.Helper()
	h, _, err := Build(Config{
		Listen:    "127.0.0.1:0",
		MaxBodyMB: 1,
		// A database is present and auth is not required: no key is issued, and
		// none is needed.
		Database:  DatabaseConfig{URL: "postgres://configured-but-unused"},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: "demo", BaseURL: engineURL}},
	}, db, detail.Nop{}, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return h
}

// assertMetered polls rather than reading once: settlement is asynchronous by
// design, because it runs after the client has been answered and must not be
// able to hold that answer up.
func assertMetered(t *testing.T, db *sqlstore.DB) {
	t.Helper()
	for i := 0; i < 50; i++ {
		var n int
		if err := db.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM usage_events`).Scan(&n); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the request was served but nothing reached the ledger")
}
