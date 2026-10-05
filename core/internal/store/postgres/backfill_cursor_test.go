package postgres

import (
	"context"
	"testing"
	"time"
)

// The cursor is the only state the backfill keeps, and it is the part that can
// lose rows without saying so. These tests are together because they are one
// property: a restart continues, and no position loses a row.

// A backfill that re-copied everything on every start would make restarting a
// gateway scan the whole ledger, on the one datastore that must never be slow.
func TestTheBackfillResumesRatherThanStartingOver(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	base := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	first := usageFixture(t, db, "acme", base, base.Add(2*time.Hour))

	var firstRun collector
	if _, err := BackfillDetail(context.Background(), db, &firstRun); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if len(firstRun.records) != len(first) {
		t.Fatalf("first run copied %d, want %d", len(firstRun.records), len(first))
	}

	// More usage arrives, then a second run.
	second := usageFixture(t, db, "acme", base.Add(2*time.Hour), base.Add(4*time.Hour))
	var secondRun collector
	if _, err := BackfillDetail(context.Background(), db, &secondRun); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if len(secondRun.records) != len(second) {
		t.Fatalf("second run copied ids %v, want %v -- it re-read the first ones",
			secondRun.ids(), second)
	}
	for i, id := range secondRun.ids() {
		if id != second[i] {
			t.Fatalf("second run copied %v, want %v", secondRun.ids(), second)
		}
	}
}

// Every other test here fits inside a single page, which is exactly why a
// cursor that skipped a row at each page boundary went unnoticed. This one
// deliberately crosses the boundary and counts every row that came back.
func TestTheBackfillCrossesAPageBoundaryWithoutLosingARow(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	const rows = detailPageSize + 1
	mustExec(t, db, `
		INSERT INTO usage_events (tenant_id, model, endpoint_id, prompt_tokens,
			completion_tokens, amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at)
		SELECT 'acme', 'qwen-7b', 'fleet/qwen-7b', 10, 5, 0, true, 'engine', false,
		       0, 50, false, now()
		  FROM generate_series(1, $1)`, rows)

	var sink collector
	copied, err := BackfillDetail(context.Background(), db, &sink)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if copied != rows {
		t.Fatalf("copied %d, want %d -- a page boundary lost rows", copied, rows)
	}

	// The cursor must land on the last row, not past it: a cursor one too high
	// would make the next run skip that row forever.
	cursor, err := db.DetailCursor(context.Background())
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	var highest int64
	if err := db.pool.QueryRow(context.Background(),
		`SELECT max(id) FROM usage_events`).Scan(&highest); err != nil {
		t.Fatalf("highest id: %v", err)
	}
	if cursor != highest {
		t.Fatalf("cursor is %d, want the highest id %d", cursor, highest)
	}
}
