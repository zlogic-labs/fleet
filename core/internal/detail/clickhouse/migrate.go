package clickhouse

import (
	"context"
	"fmt"
)

// The statements are Go values rather than a .sql file for one reason: the HTTP
// interface refuses multi-statement input by default, so a file would need a
// splitter in front of it, and a splitter is a parser that can be wrong. Each
// statement here is named in the error it produces, which is what makes a
// partially applied schema diagnosable.

// ddl are the statements that create a usable detail table, in order. They are
// idempotent, so Migrate can run on every start.
var ddl = []struct{ name, query string }{
	{"table", `
CREATE TABLE IF NOT EXISTS usage_detail
(
    -- The authoritative row id, so a reconciliation can compare the two stores
    -- by identity instead of by matching on content. Zero when the gateway
    -- pushed a record before the ledger returned its id; the backfill fills
    -- those in.
    ledger_id      Int64,

    occurred_at    DateTime64(3, 'UTC'),
    tenant         LowCardinality(String),
    project        LowCardinality(String),
    key_id         String,
    model          LowCardinality(String),
    endpoint       LowCardinality(String),
    price_book     String,

    prompt_tokens       Int64,
    completion_tokens  Int64,

    -- Nullable for the same reason the ledger's are: an engine that reports
    -- totals without a breakdown has said nothing, and a non-nullable column
    -- here would put the mirror at odds with the book it is supposed to agree
    -- with -- every reconciliation of the two would then disagree about a row
    -- where the engine simply omitted a field.
    cached_tokens      Nullable(Int64),
    reasoning_tokens   Nullable(Int64),

    -- Micro-units, exactly as the ledger stores them. Money is never a float and
    -- never a rounded decimal: a rounding rule that differed between the two
    -- stores would turn every comparison between them into a false alarm.
    amount_micro  Int64,

    -- engine / counted / reserved. An Enum rather than a String because the
    -- metering audit groups on it, and an unknown value there is a bug worth
    -- failing on rather than a value to store.
    usage_source  Enum8('engine' = 1, 'counted' = 2, 'reserved' = 3),
    usage_known   Bool,
    truncated     Bool,

    ttft_ms        Int64,
    duration_ms    Int64,
    streamed       Bool,

    -- What the engine said about its own timing, as opposed to what the
    -- gateway measured from the bytes. Nullable for the same reason the
    -- breakdown above is: null is what an engine that does not publish
    -- per-request timings returns, and zero is the answer that would make an
    -- unmeasured queue look like an idle one.
    engine_queue_ms   Nullable(Float64),
    engine_ttft_ms    Nullable(Float64),
    engine_decode_ms  Nullable(Float64),

    -- One skip index per query the ordering key cannot serve, and no others.
    --
    -- The metering audit groups by endpoint. Endpoint is not in the ordering key
    -- because putting it there would break the tenant-then-model grouping that
    -- two of the three questions depend on.
    INDEX idx_endpoint endpoint TYPE set(256) GRANULARITY 4
)
ENGINE = MergeTree
-- Date first, then the two columns every question groups by.
--
-- ClickHouse has no secondary index in the relational sense: a query reads a
-- prefix of the ordering key and scans everything else. Putting tenant or model
-- first would make the leading column useless for every query that starts with
-- a date range -- which is all of them. So a month-long close prunes to that
-- month before it looks at any other column.
--
-- There is deliberately no pre-aggregated projection here. One was written and
-- then measured: reading the projection's query cost the same rows as reading the
-- table, because the projection's GROUP BY names an alias and the planner
-- matches on the expression. It was removed rather than left in place, since a
-- projection that silently does nothing still costs a write amplification and
-- disk on every insert while making the schema look optimised.
--
-- What was measured instead, on a real server: the ordering key's date prefix
-- reads one granule of three for a one-hour range, and idx_endpoint appears in
-- the plan for the audit query.
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (toDate(occurred_at), tenant, model)
SETTINGS index_granularity = 8192`},
}

// Migrate creates the schema, reporting which statement failed.
//
// A partial schema is worse than none here: it would make queries fail in ways
// that look like missing data. So a failure is loud and the statements stop.
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+s.db); err != nil {
		return fmt.Errorf("creating the database: %w", err)
	}
	for _, stmt := range ddl {
		if err := s.Exec(ctx, stmt.query); err != nil {
			return fmt.Errorf("creating the %s: %w", stmt.name, err)
		}
	}
	return s.widen(ctx)
}

// widen brings an existing table up to the current column types.
//
// CREATE TABLE IF NOT EXISTS is "create it if it is missing", not "make it
// agree". A table written by an earlier version keeps its old shape forever,
// and the symptom is the mirror quietly disagreeing with the ledger: a nullable
// breakdown written into a non-nullable column arrives as a zero, so every row
// whose engine omitted the field reads back as "no cached tokens" -- the exact
// claim the ledger was changed to stop making.
//
// The stale projection has to go first, and not as a formality: ClickHouse
// refuses to widen a column a projection reads, with CANNOT_CONVERT_TYPE and a
// message about projection accounting. That projection was measured to read
// exactly as many rows as the table and then removed from the DDL, so dropping
// it loses nothing that was ever working.
//
// MODIFY COLUMN here rewrites metadata, not parts, which is the same line the
// ledger's own migrations are drawn on: a statement that does not rewrite data
// belongs in the schema; one that does belongs in a migration tool.
func (s *Store) widen(ctx context.Context) error {
	for _, stmt := range widenDDL {
		if err := s.Exec(ctx, fmt.Sprintf(stmt.query, s.db)); err != nil {
			return fmt.Errorf("widening the %s: %w", stmt.name, err)
		}
	}
	for _, stmt := range addColumnDDL {
		if err := s.Exec(ctx, fmt.Sprintf(stmt.query, s.db)); err != nil {
			return fmt.Errorf("adding the %s: %w", stmt.name, err)
		}
	}
	return nil
}

var widenDDL = []struct{ name, query string }{
	// The projection an earlier version declared and a later one removed.
	{"stale projection", "ALTER TABLE %s.usage_detail DROP PROJECTION IF EXISTS accounting"},
	{"usage_detail breakdown", "ALTER TABLE %s.usage_detail MODIFY COLUMN cached_tokens Nullable(Int64)"},
	{"usage_detail reasoning", "ALTER TABLE %s.usage_detail MODIFY COLUMN reasoning_tokens Nullable(Int64)"},
}

// Missing columns are added rather than widened: an ADD COLUMN does not
// rewrite the parts, which is the same line the ledger draws, and a table whose
// predecessor never had them is otherwise permanently the wrong shape.
var addColumnDDL = []struct{ name, query string }{
	{"usage_detail queue", "ALTER TABLE %s.usage_detail ADD COLUMN IF NOT EXISTS engine_queue_ms Nullable(Float64)"},
	{"usage_detail engine ttft", "ALTER TABLE %s.usage_detail ADD COLUMN IF NOT EXISTS engine_ttft_ms Nullable(Float64)"},
	{"usage_detail engine decode", "ALTER TABLE %s.usage_detail ADD COLUMN IF NOT EXISTS engine_decode_ms Nullable(Float64)"},
}
