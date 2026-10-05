package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// collector is a detail.Sink that keeps what it was given, standing in for the
// ClickHouse writer so the backfill can be tested against the ledger alone.
type collector struct {
	records []billing.Record
	// flushAt is drained by the test so the assertions can run without waiting
	// on the queue's own timer.
	closed bool
}

func (c *collector) Enqueue(r billing.Record) { c.records = append(c.records, r) }
func (c *collector) Close() error             { c.closed = true; return nil }

func (c *collector) ids() []int64 {
	out := make([]int64, len(c.records))
	for i, r := range c.records {
		out[i] = r.LedgerID
	}
	return out
}

func usageFixture(t *testing.T, db *DB, tenant string, from, to time.Time) []int64 {
	t.Helper()
	var ids []int64
	for at := from; at.Before(to); at = at.Add(time.Hour) {
		var id int64
		err := db.pool.QueryRow(context.Background(), `
			INSERT INTO usage_events (
				tenant_id, project_id, key_id, model, endpoint_id,
				prompt_tokens, completion_tokens, cached_tokens,
				amounts_micro, usage_known, usage_source, truncated,
				ttft_ms, duration_ms, streamed, occurred_at)
			VALUES ($1, $1 || '/research', 'k', 'qwen-7b', 'fleet/qwen-7b',
			        100, 20, 40, 314, true, 'engine', false, 120, 900, true, $2)
			RETURNING id`, tenant, at).Scan(&id)
		if err != nil {
			t.Fatalf("insert usage: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func resetDetailState(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `DELETE FROM detail_state`)
	mustExec(t, db, `DELETE FROM usage_events`)
}

// The point of the backfill is that the ledger can supply the rows the mirror
// lost. It is the only thing that makes the mirror's drop policy acceptable, so
// it is tested against the real table rather than a fixture.
func TestTheBackfillCopiesTheLedger(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	want := usageFixture(t, db, "acme", base, base.Add(4*time.Hour))
	if len(want) != 4 {
		t.Fatalf("fixture wrote %d rows", len(want))
	}

	var sink collector
	copied, err := BackfillDetail(context.Background(), db, &sink)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if copied != len(want) {
		t.Fatalf("copied %d, want %d", copied, len(want))
	}
	got := sink.ids()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("copied ids %v, want %v", got, want)
		}
	}
	// The scope must come across, not merely the id. These three columns are
	// nullable -- an empty project is not the same claim as a named one -- so a
	// reader that flattened them all to "" would still pass every id assertion
	// above while moving every scoped request in the mirror into one bucket.
	for i, r := range sink.records {
		if r.Tenant != "acme" || r.Project != "acme/research" || r.KeyID != "k" {
			t.Fatalf("row %d arrived as %s/%s/%s, want acme/acme/research/k",
				i, r.Tenant, r.Project, r.KeyID)
		}
	}
}

func TestTheBackfillOnAnEmptyLedgerIsNotAnError(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	var sink collector
	copied, err := BackfillDetail(context.Background(), db, &sink)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if copied != 0 || len(sink.records) != 0 {
		t.Fatalf("copied %d records into an empty ledger", copied)
	}
}

// A gap in the id sequence -- which a rollback leaves behind -- must not stop the
// walk. The rows are not coming back, so waiting for them is waiting forever.
func TestTheBackfillWalksPastAGapInTheSequence(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	ids := usageFixture(t, db, "acme", base, base.Add(3*time.Hour))
	if len(ids) < 3 {
		t.Fatalf("need at least three rows, got %d", len(ids))
	}
	// Make room for a hole by pushing the last row's id up.
	mustExec(t, db, `UPDATE usage_events SET id = id + 10000 WHERE id = $1`, ids[2])
	// A sequence that will not collide with the moved row.
	mustExec(t, db, `SELECT setval('usage_events_id_seq', (SELECT max(id) + 1000 FROM usage_events))`)

	var sink collector
	copied, err := BackfillDetail(context.Background(), db, &sink)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if copied != 3 {
		t.Fatalf("copied %d, want 3 -- the gap stopped the walk", copied)
	}
}

// The moved row above was the highest id, so the walk past the hole had nothing
// left to find. A hole in the *middle* is the case that matters: skipping to
// the ceiling on an empty page is only correct when there is genuinely nothing
// above it, and this is the assertion that says so.
func TestTheBackfillFindsTheRowsAboveAHoleInTheSequence(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	ids := usageFixture(t, db, "acme", base, base.Add(5*time.Hour))
	if len(ids) != 5 {
		t.Fatalf("need five rows, got %d", len(ids))
	}
	// Push row two far up so rows three to five sit *below* it and the ids
	// between one and two become a hole.
	mustExec(t, db, `UPDATE usage_events SET id = id + 10000 WHERE id = $1`, ids[1])
	mustExec(t, db, `SELECT setval('usage_events_id_seq', (SELECT max(id) + 1000 FROM usage_events))`)

	var sink collector
	copied, err := BackfillDetail(context.Background(), db, &sink)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if copied != len(ids) {
		t.Fatalf("copied %d, want %d -- the walk gave up at the hole", copied, len(ids))
	}
	// Every row the table holds, including the moved one. The expectation is read
	// from the table rather than from the fixture, because the move changed one
	// of the ids and comparing against the pre-move list would report a row as
	// missing when it arrived under its new id.
	want := idsInTable(t, db)
	for _, id := range sink.ids() {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("these rows never arrived: %v", want)
	}
}

// idsInTable is the set of ids currently stored, for a test that wants to know
// which rows the walk failed to deliver.
func idsInTable(t *testing.T, db *DB) map[int64]bool {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), `SELECT id FROM usage_events`)
	if err != nil {
		t.Fatalf("read ids: %v", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ids: %v", err)
	}
	return out
}

// A row with no token breakdown stored is not the same as a row with zero
// tokens: the first means the engine reported nothing, the second means it
// reported zeros. The mirror must not invent a breakdown for the first.
func TestTheBackfillKeepsAnAbsentBreakdownAbsent(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	at := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	mustExec(t, db, `
		INSERT INTO usage_events (tenant_id, model, endpoint_id, prompt_tokens,
			completion_tokens, amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at)
		VALUES ('acme', 'qwen-7b', 'fleet/qwen-7b', 10, 5, 0, false, 'counted', false, 0, 50, false, $1)`, at)

	var sink collector
	if _, err := BackfillDetail(context.Background(), db, &sink); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(sink.records) != 1 {
		t.Fatalf("copied %d records", len(sink.records))
	}
	got := sink.records[0]
	if got.Usage.PromptTokensDetails != nil {
		t.Errorf("a row with no breakdown came back with %+v", got.Usage.PromptTokensDetails)
	}
	// The scope must survive the copy. The mirror groups by it, and a
	// reader that flattened NULL to the empty string would quietly move every
	// unattributed request into one bucket instead of reporting it unattributed.
	if got.Project != "" || got.KeyID != "" {
		t.Errorf("a row with no project or key came back with %q/%q", got.Project, got.KeyID)
	}
	if got.UsageSource != billing.SourceCounted {
		t.Errorf("usage source is %q, want counted", got.UsageSource)
	}
	// Milliseconds in the database, a duration on the way out.
	if got.Duration != 50*time.Millisecond {
		t.Errorf("duration is %v, want 50ms", got.Duration)
	}
}
