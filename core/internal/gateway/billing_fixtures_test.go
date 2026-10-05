package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// Fixtures that seed the database and read the ledger back. The builders live
// in billing_build_test.go.

// seedBudget writes a money ceiling: micro-units per hour, which is the unit
// the units dimension measures in.
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
		       usage_source, prompt_tokens, completion_tokens, cached_tokens,
		       reasoning_tokens
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
			// Nullable, because the ledger stores the absence. A row whose engine
			// omitted the breakdown has to read back as an absent breakdown, or
			// this helper would report "zero cached tokens" for it.
			cached    *int64
			reasoning *int64
		)
		if err := rows.Scan(&r.Tenant, &r.Project, &r.KeyID, &r.Model, &r.PriceBook,
			&r.Amount, &r.UsageKnown, &source,
			&r.Usage.PromptTokens, &r.Usage.CompletionTokens,
			&cached, &reasoning); err != nil {
			t.Fatalf("scan the ledger: %v", err)
		}
		if cached != nil {
			r.Usage.PromptTokensDetails = &openai.PromptTokensDetails{
				CachedTokens: int(*cached),
			}
		}
		if reasoning != nil {
			r.Usage.CompletionTokensDetails = &openai.CompletionTokensDetails{
				ReasoningTokens: int(*reasoning),
			}
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
