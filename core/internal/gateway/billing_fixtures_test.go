package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
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

// buildBillingTokenLimit builds a gateway for a tenant whose per-minute token
// ceiling is tokenLimit. It does not set DefaultMaxTokens, so a test that
// reasons about the reservation must use buildPricedWiredWithMax instead of
// arriving here and inheriting whatever the handler defaults to.
func buildBillingTokenLimit(t *testing.T, db *sqlstore.DB, tokenLimit int,
	engineURL, label string) (http.Handler, string) {
	t.Helper()
	seedTenant(t, db, "acme", 100, tokenLimit)
	key := seedKey(t, db, "acme", "research", label)
	return buildPricedWired(t, db, engineURL), key
}

// buildBudgeted assembles a gateway whose tenant and project both hold a budget
// of the given number of micro-units per hour, and a key in that project.
//
// The figure is in micro-units, which is what the units dimension measures and
// what the column stores: one unit is a million of them. Naming a parameter
// "units" and passing 1000 reads as a dollar budget and is a thousandth of one,
// which is the kind of test that passes for the wrong reason.
//
// Both levels carry the same rule so the partition never binds before the
// envelope does, which keeps a test about one rule from failing on the other.
// The rule is written to budget_rules rather than a column on the tenant: a
// budget is a dimension, a window and a scope, and one bigint column could only
// ever have been the first of those three.
func buildBudgeted(t *testing.T, db *sqlstore.DB, budgetMicro int64, engineURL, label string) (http.Handler, string) {
	t.Helper()
	// seedTenant and seedKey create the tenant and the project; the rules go in
	// afterwards because neither takes one.
	seedTenant(t, db, "acme", 100, 1_000_000)
	key := seedKey(t, db, "acme", "research", label)
	seedBudget(t, db, "tenant", "acme", budgetMicro)
	seedBudget(t, db, "project", "acme/research", budgetMicro)
	return buildPricedWired(t, db, engineURL), key
}

// buildBudgetedTokens is buildBudgeted for a token ceiling instead of a money
// one, so a test can drive two dimensions against the same tenant.
func buildBudgetedTokens(t *testing.T, db *sqlstore.DB, tokens int64, engineURL, label string) (http.Handler, string) {
	t.Helper()
	seedTenant(t, db, "acme", 100, 1_000_000)
	key := seedKey(t, db, "acme", "research", label)
	seedBudgetDimension(t, db, "tenant", "acme", quota.TokensTotal, tokens)
	seedBudgetDimension(t, db, "project", "acme/research", quota.TokensTotal, tokens)
	return buildPricedWired(t, db, engineURL), key
}

// buildPricedWired is buildWired with a model that has a price, because the
// billing tests need one and the auth tests do not care.
func buildPricedWired(t *testing.T, db *sqlstore.DB, engineURL string) http.Handler {
	t.Helper()
	return buildPricedWiredWithMax(t, db, engineURL, 0)
}

// buildPricedWiredWithMax builds the same gateway with an explicit
// DefaultMaxTokens. A zero means "whatever the handler defaults to", which is
// right for tests that are not about the reservation.
//
// Tests that are about the reservation must set it. That one did not, and its
// arithmetic depended on the handler's default happening to sit in a narrow
// window -- so changing that default from 1024 to 4096, which is what the
// configuration says a request may cost, silently turned a test about refunds
// into a test about the limit.
func buildPricedWiredWithMax(t *testing.T, db *sqlstore.DB, engineURL string, maxTokens int) http.Handler {
	t.Helper()
	cfg := Config{
		Listen: "127.0.0.1:0", MaxBodyMB: 1,
		DefaultMaxTokens: maxTokens,
		Auth:             AuthConfig{Required: true},
		Database:         DatabaseConfig{URL: "postgres://configured-but-unused"},
		Upstreams:        []UpstreamConfig{{ID: "e1", Model: pricedModel, BaseURL: engineURL}},
	}
	h, _, err := Build(cfg, db, entitlement.Community(), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return h
}

func seedBudget(t *testing.T, db *sqlstore.DB, kind, id string, units int64) {
	t.Helper()
	seedBudgetDimension(t, db, kind, id, quota.Units, units)
}

func seedBudgetDimension(t *testing.T, db *sqlstore.DB, kind, id string, d quota.Dimension, limit int64) {
	t.Helper()
	mustExecWired(t, db,
		`INSERT INTO budget_rules (id, scope_kind, scope_id, dimension,
		                           limit_value, window_seconds, resolution_seconds)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		kind+"/"+id+"/"+string(d), kind, id, string(d), limit, 3600, 60)
}

// countRows is how many ledger rows exist, for the "nothing was billed" checks.
func countRows(t *testing.T, db *sqlstore.DB) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatalf("count the ledger: %v", err)
	}
	return n
}

func mustExecWired(t *testing.T, db *sqlstore.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// seedPrice writes a price book for demo: 1M input tokens at 1000 quota units
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

// silentEngine answers with text but no usage at all, which is what an engine
// that honours neither stream_options.include_usage nor a usage field looks
// like. It is the case the gateway has to measure for itself.
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

// muteEngine answers with no usage and no text either: a refusal, an empty
// completion, or a stream cut before its first frame. There is nothing to
// count here, so the reservation is the only figure left.
func muteEngine(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"demo",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":""},` +
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
	UsageSource            billing.Source
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
		       usage_source, prompt_tokens, completion_tokens, cached_tokens
		  FROM usage_events`)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	defer rows.Close()

	var out []storedRecord
	for rows.Next() {
		var (
			r      storedRecord
			source string
			cached int
		)
		if err := rows.Scan(&r.Tenant, &r.Project, &r.KeyID, &r.Model, &r.PriceBook,
			&r.Amount, &r.UsageKnown, &source,
			&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &cached); err != nil {
			t.Fatalf("scan the ledger: %v", err)
		}
		r.UsageSource = billing.Source(source)
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
