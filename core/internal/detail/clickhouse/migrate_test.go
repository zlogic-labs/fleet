package clickhouse

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A table an earlier version wrote keeps its old column types, because
// CREATE TABLE IF NOT EXISTS means "if it is missing" and nothing else. The
// mirror then reads back a zero for every row whose engine omitted the token
// breakdown -- the claim the ledger was changed to stop making, reappearing one
// store downstream.
//
// This builds exactly that starting point and runs the migration over it, which
// is the only way to catch it: on a fresh database both the old and the new
// DDL produce the same table.
func TestMigrateWidensAnExistingTable(t *testing.T) {
	base := testStore(t)
	ctx := context.Background()

	scratch := fmt.Sprintf("fleet_widen_%d", time.Now().UnixNano())
	if err := base.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatalf("create the scratch database: %v", err)
	}
	t.Cleanup(func() { _ = base.Exec(context.Background(), "DROP DATABASE "+scratch) })

	s := &Store{db: scratch, conn: base.conn}
	t.Cleanup(func() { _ = s.Close() })

	// The shape an earlier version left behind, projection included. The
	// projection is in the fixture on purpose: ClickHouse refuses to widen a
	// column a projection reads, so a test table without one would pass while
	// every real upgrade failed.
	if err := s.Exec(ctx, `CREATE TABLE `+scratch+`.usage_detail (
		occurred_at DateTime64(3, 'UTC'),
		tenant LowCardinality(String),
		model LowCardinality(String),
		cached_tokens Int64,
		reasoning_tokens Int64,
		PROJECTION accounting (SELECT tenant, sum(cached_tokens) GROUP BY tenant)
	) ENGINE = MergeTree ORDER BY (toDate(occurred_at), tenant, model)`); err != nil {
		t.Fatalf("create the old-shaped table: %v", err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"cached_tokens", "reasoning_tokens"} {
		var kind string
		if err := s.QueryRow(ctx,
			`SELECT type FROM system.columns
			  WHERE database = $1 AND table = 'usage_detail' AND name = $2`,
			scratch, col).Scan(&kind); err != nil {
			t.Fatalf("read the type of %s: %v", col, err)
		}
		if kind != "Nullable(Int64)" {
			t.Errorf("%s is %q after migrating an old table, want Nullable(Int64)", col, kind)
		}
	}
}
