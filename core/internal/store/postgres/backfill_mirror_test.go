package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
)

// The collector and the ledger fixtures live in backfill_test.go; only the two
// tests that need a real ClickHouse are here, because a suite that needs two
// databases is a suite people stop running.

func testClickHouse(t *testing.T) (chURL, chDB, chUser string) {
	t.Helper()
	url := os.Getenv("FLEET_TEST_CLICKHOUSE_URL")
	if url == "" {
		return "", "", ""
	}
	database := os.Getenv("FLEET_TEST_CLICKHOUSE_DATABASE")
	if database == "" {
		database = "fleet_detail"
	}
	return url, database, os.Getenv("FLEET_TEST_CLICKHOUSE_USER")
}

// usageFixture inserts rows directly, so the test controls the ids and the
// The end-to-end version: rows land in the ledger, are mirrored into a real
// ClickHouse, and are readable through the report query. This is the only test
// that would catch the two stores disagreeing about a column.
func TestTheMirrorServesWhatTheLedgerRecorded(t *testing.T) {
	url, database, user := testClickHouse(t)
	db := testDB(t)
	resetDetailState(t, db)

	base := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)
	usageFixture(t, db, "acme", base, base.Add(3*time.Hour))

	store, err := clickhouse.Open(context.Background(), clickhouse.Config{
		URL: url, Database: database, User: user,
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate clickhouse: %v", err)
	}
	if err := store.Exec(context.Background(), "TRUNCATE TABLE "+store.Database()+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	var sink collector
	if _, err := BackfillDetail(context.Background(), db, &sink); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := clickhouse.NewWriter(store).Write(context.Background(), sink.records); err != nil {
		t.Fatalf("write to clickhouse: %v", err)
	}

	// Read back through the same query the reports use.
	rows, err := store.Query(context.Background(),
		`SELECT tenant, model, sum(prompt_tokens), sum(completion_tokens), sum(cached_tokens),
		        sum(amount_micro), count()
		 FROM `+store.Database()+`.usage_detail
		 WHERE occurred_at >= $1 AND occurred_at < $2
		 GROUP BY tenant, model`, base.Add(-time.Hour), base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the mirror served no rows the ledger has")
	}
	var tenant, model string
	var prompt, completion, cached, amount int64
	var n uint64
	if err := rows.Scan(&tenant, &model, &prompt, &completion, &cached, &amount, &n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tenant != "acme" || model != "qwen-7b" || n != 3 {
		t.Fatalf("got %s/%s with %d rows", tenant, model, n)
	}
	if prompt != 300 || completion != 60 || cached != 120 || amount != 942 {
		t.Errorf("got prompt=%d completion=%d cached=%d amount=%d, want 300/60/120/942",
			prompt, completion, cached, amount)
	}
}

// A row whose engine reported totals and no breakdown must reach the mirror
// with the breakdown still absent. The other mirror test cannot catch a writer
// that stores zero here, because every row in it has a breakdown -- so this one
// writes the row the writer would get wrong and asks the server.
func TestTheMirrorKeepsAnAbsentBreakdownAbsent(t *testing.T) {
	url, database, user := testClickHouse(t)
	db := testDB(t)
	resetDetailState(t, db)

	at := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	mustExec(t, db, `
		INSERT INTO usage_events (tenant_id, model, endpoint_id, prompt_tokens,
			completion_tokens, amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at)
		VALUES ('acme', 'qwen-7b', 'fleet/qwen-7b', 10, 5, 0, false, 'counted', false,
		        0, 50, false, $1)`, at)

	store, err := clickhouse.Open(context.Background(), clickhouse.Config{
		URL: url, Database: database, User: user,
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.Exec(context.Background(), "TRUNCATE TABLE "+store.Database()+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	var sink collector
	if _, err := BackfillDetail(context.Background(), db, &sink); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := clickhouse.NewWriter(store).Write(context.Background(), sink.records); err != nil {
		t.Fatalf("write: %v", err)
	}

	var cached, reasoning *int64
	if err := store.QueryRow(context.Background(),
		`SELECT cached_tokens, reasoning_tokens FROM `+store.Database()+`.usage_detail
		 WHERE occurred_at >= $1 AND occurred_at < $2 LIMIT 1`,
		at.Add(-time.Hour), at.Add(time.Hour)).Scan(&cached, &reasoning); err != nil {
		t.Fatalf("query the mirror: %v", err)
	}
	if cached != nil || reasoning != nil {
		t.Fatalf("the mirror stored a breakdown the engine never sent: cached=%v reasoning=%v",
			cached, reasoning)
	}
}
