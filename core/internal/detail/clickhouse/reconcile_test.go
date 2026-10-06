package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func TestTheReplicaTallyCountsWhatWasMirrored(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	seed(t, s, at, 3)

	got, err := s.DetailTally(context.Background(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("DetailTally: %v", err)
	}
	want := billing.Tally{Records: 3, PromptTokens: 300, CompletionTokens: 60, CachedTokens: 120, AmountMicro: 942}
	if got != want {
		t.Fatalf("tally is %+v, want %+v", got, want)
	}
}

func TestTheReplicaGroupsUseTheSameKeyTheLedgerUses(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	records := []billing.Record{
		sample(at, "acme", "qwen-7b"),
		sample(at.Add(time.Second), "acme", "qwen-7b"),
		sample(at.Add(2*time.Second), "globex", "llama-3"),
	}
	for i := range records {
		records[i].LedgerID = int64(i + 1)
	}
	if err := NewWriter(s).Write(context.Background(), records); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.DetailGroups(context.Background(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("DetailGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	if got[0].Key != billing.GroupKey("acme", "qwen-7b") {
		t.Errorf("first key is %q, want %q", got[0].Key, billing.GroupKey("acme", "qwen-7b"))
	}
	if got[1].Key != billing.GroupKey("globex", "llama-3") {
		t.Errorf("second key is %q, want %q", got[1].Key, billing.GroupKey("globex", "llama-3"))
	}
	if got[0].Records != 2 || got[0].AmountMicro != 628 {
		t.Errorf("acme/qwen-7b is %+v, want 2 records and 628 micro", got[0].Tally)
	}
}

func TestAReplicaGroupWithNoBreakdownReadsAsZero(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	r := sample(at, "acme", "qwen-7b")
	// An engine that reported totals without a breakdown. The column is
	// nullable for exactly this row, and SUM over a group where every row is
	// NULL is NULL rather than zero, so the query has to COALESCE or the whole
	// reconciliation fails on a month that is perfectly healthy.
	r.Usage.PromptTokensDetails = nil
	if err := NewWriter(s).Write(context.Background(), []billing.Record{r}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.DetailTally(context.Background(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("DetailTally: %v", err)
	}
	if got.CachedTokens != 0 {
		t.Errorf("cached got %d, want 0", got.CachedTokens)
	}
	if got.Records != 1 || got.AmountMicro != 314 {
		t.Errorf("tally is %+v, want one record and 314 micro", got)
	}
}

func TestTheReplicaTallyIsEmptyWhenNothingWasMirrored(t *testing.T) {
	s := testStore(t)
	truncate(t, s)

	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	got, err := s.DetailTally(context.Background(), at, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("DetailTally: %v", err)
	}
	// An empty window is a zero tally, not an error and not a missing row: the
	// reconciliation has to be able to compare a ledger with no usage against a
	// replica with no usage and call it agreement.
	if got != (billing.Tally{}) {
		t.Fatalf("empty window returned %+v", got)
	}
}
