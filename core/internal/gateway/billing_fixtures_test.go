package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/entitlement"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Fixtures for the settlement tests. Split out from billing_test.go so each
// file stays readable; the assertions are there.

// buildBilling assembles a gateway with a ledger and a price book, plus a key
// that exists only in the database, and returns both.
//
// The key is seeded rather than declared in the config because a gateway
// pointed at a database ignores the config's key list entirely — that is the
// whole point of the wiring, and a test that passed a config key here would be
// testing a path the deployment does not use.
func buildBilling(t *testing.T, db *sqlstore.DB, engineURL, label string) (http.Handler, string) {
	t.Helper()
	return buildBillingTokenLimit(t, db, 1_000_000, engineURL, label)
}

func buildBillingTokenLimit(t *testing.T, db *sqlstore.DB, tokenLimit int,
	engineURL, label string) (http.Handler, string) {
	t.Helper()
	seedTenant(t, db, "acme", 100, tokenLimit)
	key := seedKey(t, db, "acme", "research", label)
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		Auth:      AuthConfig{Required: true},
		Upstreams: []UpstreamConfig{{ID: "e1", Model: pricedModel, BaseURL: engineURL}},
	}
	h, _, err := Build(cfg, db, entitlement.Community(), discardLogger(), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return h, key
}

// seedPrice writes a price book for demo: 1M input tokens at 1000 micro per
// unit, 1M output at 2000.
func seedPrice(t *testing.T, db *sqlstore.DB) {
	t.Helper()
	prices := sqlstore.NewPriceStore(db, 0)
	if _, err := prices.PutPrice(context.Background(), billing.Price{
		Model: pricedModel,
		Rate:  billing.Rate{Input: 1000, Output: 2000, Cached: 100},
	}, nowHourAgo()); err != nil {
		t.Fatalf("put price: %v", err)
	}
}

// silentEngine answers with no usage at all, which is what a stream cut short
// looks like and what P6's fallback exists for.
func silentEngine(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"demo",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
			`"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// storedRecord is the ledger row as the assertions want to read it.
type storedRecord struct {
	Tenant, Project, KeyID string
	Model                  string
	PriceBook              string
	Amount                 int64
	UsageKnown             bool
	Usage                  openai.Usage
}

// onlyRecord returns the single ledger row, failing if there is not exactly
// one. "Exactly one" is the assertion: a settlement that wrote twice would
// double-bill, and a check of "at least one" would not notice.
func onlyRecord(t *testing.T, db *sqlstore.DB) storedRecord {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `
		SELECT tenant_id, COALESCE(project_id,''), COALESCE(key_id,''),
		       model, COALESCE(price_book_id,''), amounts_micro, usage_known,
		       prompt_tokens, completion_tokens, cached_tokens
		  FROM usage_events`)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	defer rows.Close()

	var out []storedRecord
	for rows.Next() {
		var (
			r      storedRecord
			cached int
		)
		if err := rows.Scan(&r.Tenant, &r.Project, &r.KeyID, &r.Model, &r.PriceBook,
			&r.Amount, &r.UsageKnown,
			&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &cached); err != nil {
			t.Fatalf("scan the ledger: %v", err)
		}
		r.Usage.TotalTokens = r.Usage.PromptTokens + r.Usage.CompletionTokens
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the ledger: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("the ledger holds %d rows, want exactly 1", len(out))
	}
	return out[0]
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func nowHourAgo() time.Time { return time.Now().Add(-time.Hour) }
