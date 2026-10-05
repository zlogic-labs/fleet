package clickhouse

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// testStore opens the detail database, or skips.
//
// Skipping is correct rather than a convenience: the store is optional by
// design, and a suite that failed without it would mean the tests were really
// asserting that the operator installed something.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("FLEET_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("FLEET_TEST_CLICKHOUSE_URL is not set")
	}
	s, err := Open(context.Background(), Config{
		URL:      url,
		Database: os.Getenv("FLEET_TEST_CLICKHOUSE_DATABASE"),
		User:     os.Getenv("FLEET_TEST_CLICKHOUSE_USER"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sample(at time.Time, tenant, model string) billing.Record {
	return billing.Record{
		LedgerID:    4242,
		Tenant:      tenant,
		Project:     tenant + "/research",
		Model:       model,
		Endpoint:    "fleet/" + model,
		Usage:       openai.Usage{PromptTokens: 100, CompletionTokens: 20, PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 40}},
		UsageKnown:  true,
		UsageSource: billing.SourceEngine,
		Amount:      billing.Amount(314),
		TTFT:        120 * time.Millisecond,
		Duration:    900 * time.Millisecond,
		Streamed:    true,
		OccurredAt:  at,
	}
}

// The store may lose data, so the schema has to be creatable more than once.
// A Migrate that failed the second time would mean a restart could break a
// deployment that was working.
func TestMigrateIsIdempotent(t *testing.T) {
	s := testStore(t)
	for i := range 3 {
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
}

func TestTheSchemaLooksLikeItWasWritten(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var created string
	if err := s.QueryRow(context.Background(),
		"SHOW CREATE TABLE "+s.db+".usage_detail").Scan(&created); err != nil {
		t.Fatalf("show create: %v", err)
	}

	// The ordering key is the design, not a default: a query reads a prefix of
	// it and scans everything else, so getting it wrong makes every date-range
	// report a full scan. A schema that created successfully with a different
	// ordering would pass every other test in this file.
	for _, want := range []string{
		"ORDER BY (toDate(occurred_at), tenant, model)",
		"PARTITION BY toYYYYMM(occurred_at)",
		"idx_endpoint",
		"Enum8('engine' = 1, 'counted' = 2, 'reserved' = 3)",
		// The breakdown has to be nullable or the mirror turns an absent
		// breakdown into a zero on the way in.
		"Nullable(Int64)",
	} {
		if !contains(created, want) {
			t.Errorf("the created table does not contain %q", want)
		}
	}

	// And it must not contain the projection that was measured to read exactly
	// as many rows as the table and then removed from the DDL. It sat in this
	// list for a while, asserting an artefact of an earlier version rather than
	// the current design -- and it passed, because the shared test database
	// still had the old projection in it. Asserting its absence is what makes
	// this test say something about the schema rather than about a leftover.
	if contains(created, "accounting") {
		t.Error("the table still carries the projection that was measured to do nothing")
	}
}

func TestWriteThenReadBackTheSameNumbers(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Exec(context.Background(), "TRUNCATE TABLE "+s.db+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	records := []billing.Record{sample(at, "acme", "qwen-7b"), sample(at.Add(time.Second), "globex", "qwen-7b")}
	if err := NewWriter(s).Write(context.Background(), records); err != nil {
		t.Fatalf("write: %v", err)
	}

	rows, err := s.Query(context.Background(),
		`SELECT count(), sum(prompt_tokens), sum(completion_tokens), sum(cached_tokens), sum(amount_micro)
		 FROM `+s.db+`.usage_detail`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var n uint64
	var prompt, completion, cached, amount int64
	if !rows.Next() {
		t.Fatal("no rows after a write")
	}
	if err := rows.Scan(&n, &prompt, &completion, &cached, &amount); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 2 || prompt != 200 || completion != 40 || cached != 80 || amount != 628 {
		t.Errorf("got n=%d prompt=%d completion=%d cached=%d amount=%d, want 2/200/40/80/628",
			n, prompt, completion, cached, amount)
	}
}

// A usage source the table does not know would fail the whole batch, taking
// every other record with it. So an unknown value has to be mapped somewhere.
func TestAnUnknownUsageSourceDoesNotFailTheBatch(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Exec(context.Background(), "TRUNCATE TABLE "+s.db+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	good := sample(at, "acme", "qwen-7b")
	bad := sample(at, "initech", "qwen-7b")
	bad.UsageSource = billing.Source("guessed")

	if err := NewWriter(s).Write(context.Background(), []billing.Record{good, bad}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var n uint64
	if err := s.QueryRow(context.Background(),
		"SELECT count() FROM "+s.db+".usage_detail").Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 2 {
		t.Errorf("got %d rows, want 2 -- the unknown source took the batch with it", n)
	}
}

// A batch larger than one insert is chunked, and the chunks must all land.
// A writer that only sent the first chunk would look correct on a small fleet
// and silently truncate a busy one's reporting.
func TestABatchLargerThanOneInsertIsFullyWritten(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Exec(context.Background(), "TRUNCATE TABLE "+s.db+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	const total = insertBatch + 37
	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	records := make([]billing.Record, total)
	for i := range records {
		records[i] = sample(at.Add(time.Duration(i)*time.Millisecond), "acme", "qwen-7b")
		records[i].LedgerID = int64(i)
	}
	if err := NewWriter(s).Write(context.Background(), records); err != nil {
		t.Fatalf("write: %v", err)
	}

	var n uint64
	if err := s.QueryRow(context.Background(),
		"SELECT count() FROM "+s.db+".usage_detail").Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != total {
		t.Errorf("wrote %d records and %d landed", total, n)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
