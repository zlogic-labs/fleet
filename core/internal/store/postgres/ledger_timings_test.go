package postgres

import (
	"context"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The write path is where a null becomes a zero, so it is the half that has to
// be covered: recording against an engine that published nothing has to leave
// the columns empty, not filled with a plausible figure. Every llama.cpp
// deployment on this fleet takes this branch, and a stored zero would say its
// requests never queued.
func TestARecordWithNoEngineTimingsStoresNothing(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	ctx := context.Background()

	if _, err := NewLedger(db).Record(ctx, billing.Record{
		Tenant: "acme", Model: "qwen-0.5b", Usage: usage(10, 2, 0), UsageKnown: true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	queue, ttft, decode := timingColumns(t, db, "qwen-0.5b")
	if queue != nil || ttft != nil || decode != nil {
		t.Errorf("a record with no engine timings stored %v/%v/%v; every engine that "+
			"publishes nothing would be filed as queueing for no time at all", queue, ttft, decode)
	}

	if _, err := NewLedger(db).Record(ctx, billing.Record{
		Tenant: "acme", Model: "qwen-7b", Usage: usage(10, 2, 0), UsageKnown: true,
		Engine: &billing.EngineTimings{QueueMS: f64(0), TTFTMS: f64(1234.5)},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	queue, ttft, decode = timingColumns(t, db, "qwen-7b")
	// A measured zero and an absent field in one row: different claims, and one
	// of them cannot be stored as the other.
	if queue == nil || *queue != 0 {
		t.Errorf("queue = %v, want a measured 0", queue)
	}
	if ttft == nil || *ttft != 1234.5 {
		t.Errorf("engine ttft = %v, want 1234.5", ttft)
	}
	if decode != nil {
		t.Errorf("decode = %v; it was not reported and must stay null", *decode)
	}
}

func timingColumns(t *testing.T, db *DB, model string) (queue, ttft, decode *float64) {
	t.Helper()
	err := db.pool.QueryRow(context.Background(),
		`SELECT engine_queue_ms, engine_ttft_ms, engine_decode_ms
		 FROM usage_events WHERE model = $1 ORDER BY id DESC LIMIT 1`, model).
		Scan(&queue, &ttft, &decode)
	if err != nil {
		t.Fatalf("reading back %s: %v", model, err)
	}
	return queue, ttft, decode
}

func f64(v float64) *float64 { return &v }

// A row whose engine published no timings must keep saying so after a round
// trip through the mirror. The failure this guards is not a crash: it is a NULL
// read as a float zero, which would copy "queued for no time" into ClickHouse
// for every engine that does not report one.
func TestAnEngineThatPublishedNothingStaysNull(t *testing.T) {
	db := testDB(t)
	resetDetailState(t, db)

	mustExec(t, db, `
		INSERT INTO usage_events (tenant_id, model, endpoint_id, prompt_tokens,
			completion_tokens, amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at,
			engine_queue_ms, engine_ttft_ms, engine_decode_ms)
		VALUES ('loud', 'qwen-7b', 'fleet/qwen-7b', 10, 5, 0, true, 'engine', false,
		        1200, 900, true, now(), 0, 1234.5, NULL)`)
	mustExec(t, db, `
		INSERT INTO usage_events (tenant_id, model, endpoint_id, prompt_tokens,
			completion_tokens, amounts_micro, usage_known, usage_source, truncated,
			ttft_ms, duration_ms, streamed, occurred_at)
		VALUES ('quiet', 'qwen-0.5b', 'fleet/qwen-0.5b', 10, 5, 0, true, 'engine', false,
		        900, 400, true, now())`)

	var sink collector
	if _, err := BackfillDetail(context.Background(), db, &sink); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	byTenant := map[string]billing.Record{}
	for _, r := range sink.records {
		byTenant[r.Tenant] = r
	}
	if len(byTenant) != 2 {
		t.Fatalf("copied %d distinct tenants, want 2", len(byTenant))
	}

	if got := byTenant["quiet"].Engine; got != nil {
		t.Errorf("a row with no engine timings came back as %+v; null must stay null", got)
	}
	got := byTenant["loud"].Engine
	if got == nil {
		t.Fatal("a row with engine timings came back with none")
	}
	// A measured zero and an absent field in one record: different claims, and
	// one of them cannot be stored as the other.
	if got.QueueMS == nil || *got.QueueMS != 0 {
		t.Errorf("queue = %v, want a measured 0", got.QueueMS)
	}
	if got.TTFTMS == nil || *got.TTFTMS != 1234.5 {
		t.Errorf("engine ttft = %v, want 1234.5", got.TTFTMS)
	}
	if got.DecodeMS != nil {
		t.Errorf("decode = %v; the column was null and must not come back as a number", *got.DecodeMS)
	}
}
