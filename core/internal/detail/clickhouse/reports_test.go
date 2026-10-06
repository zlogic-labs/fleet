package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func seed(t *testing.T, s *Store, at time.Time, n int) {
	t.Helper()
	records := make([]billing.Record, n)
	for i := range records {
		r := sample(at.Add(time.Duration(i)*time.Second), "acme", "qwen-7b")
		r.LedgerID = int64(at.Unix()) + int64(i)
		records[i] = r
	}
	if err := NewWriter(s).Write(context.Background(), records); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func truncate(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Exec(context.Background(), "TRUNCATE TABLE "+s.db+".usage_detail"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// The report query is the one the overview and the cost page issue. It once
// filtered on a `day` column that only existed inside a projection, so the
// shape the console depends on did not compile against the table at all -- and
// it failed only when run against a server, never in a test that stopped at
// building a string.
func TestTheDailyReportAnswersTheQuestionTheConsoleAsks(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	sept := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	seed(t, s, sept, 3)
	oct := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	seed(t, s, oct, 5)

	got, err := s.Daily(context.Background(),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("daily: %v", err)
	}

	// Two months, two rows. If the range is off by a day or the timezone
	// differs, September's rows land in October or disappear.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Requests != 3 || got[1].Requests != 5 {
		t.Errorf("got %d then %d requests, want 3 then 5", got[0].Requests, got[1].Requests)
	}
	if got[0].AmountMicro != 3*314 || got[1].AmountMicro != 5*314 {
		t.Errorf("amounts %d then %d, want %d then %d",
			got[0].AmountMicro, got[1].AmountMicro, 3*314, 5*314)
	}
}

func TestTheDailyReportCanBeScopedToOneTenant(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	seed(t, s, at, 4)

	if _, err := s.Daily(context.Background(), at.Add(-time.Hour), at.Add(time.Hour), "nobody"); err != nil {
		t.Fatalf("daily: %v", err)
	}
	got, err := s.Daily(context.Background(), at.Add(-time.Hour), at.Add(time.Hour), "acme")
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(got) != 1 || got[0].Requests != 4 {
		t.Fatalf("got %+v, want one row of 4 requests", got)
	}
}

// The boundary is exclusive at the top, because a period close and a report that
// disagreed about whether midnight belongs to the month would produce two
// different invoices for the same instant.
func TestTheDailyRangeExcludesItsUpperBound(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	boundary := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seed(t, s, boundary.Add(-time.Second), 1)
	seed(t, s, boundary, 1)

	september, err := s.Daily(context.Background(),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), boundary, "")
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(september) != 1 || september[0].Requests != 1 {
		t.Fatalf("september got %+v, want exactly the row before the boundary", september)
	}
}

func TestTheEndpointTotalsKeepTheThreeSourcesApart(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	engine := sample(at, "acme", "qwen-7b")
	engine.UsageSource = billing.SourceEngine
	engine.Usage.CompletionTokens = 30

	counted := sample(at.Add(time.Second), "acme", "qwen-7b")
	counted.UsageSource = billing.SourceCounted
	counted.Usage.CompletionTokens = 28

	reserved := sample(at.Add(2*time.Second), "acme", "qwen-7b")
	reserved.UsageSource = billing.SourceReserved
	reserved.Usage.CompletionTokens = 1024

	if err := NewWriter(s).Write(context.Background(), []billing.Record{engine, counted, reserved}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := s.EndpointUsage(context.Background(), at.Add(-time.Hour), at.Add(time.Hour), "fleet/qwen-7b")
	if err != nil {
		t.Fatalf("endpoint usage: %v", err)
	}

	// The whole point of the audit is comparing two independent measurements.
	// A single combined number would answer "they agree" even when there are no
	// engine rows at all, which is the case an operator most needs to see.
	if got.EngineN != 1 || got.EngineOut != 30 {
		t.Errorf("engine got n=%d out=%d, want 1/30", got.EngineN, got.EngineOut)
	}
	if got.CountedN != 1 || got.CountedOut != 28 {
		t.Errorf("counted got n=%d out=%d, want 1/28", got.CountedN, got.CountedOut)
	}
	if got.ReservedN != 1 || got.ReservedOut != 1024 {
		t.Errorf("reserved got n=%d out=%d, want 1/1024", got.ReservedN, got.ReservedOut)
	}
}

func TestTheDailyReportCountsARowWhoseBreakdownWasOmitted(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)
	r := sample(at, "acme", "qwen-7b")
	// An engine that reports totals without a breakdown, which the ledger stores
	// as NULL and the replica copies as NULL. SUM over a group where every row
	// said nothing is NULL, and scanning NULL into an int fails the whole
	// report rather than converting -- so a month served entirely by such an
	// engine would return an error where it should return a number.
	r.Usage.PromptTokensDetails = nil
	if err := NewWriter(s).Write(context.Background(), []billing.Record{r}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.Daily(context.Background(), at.Add(-time.Hour), at.Add(time.Hour), "")
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].CachedTokens != 0 {
		t.Errorf("cached got %d, want 0", got[0].CachedTokens)
	}
}
