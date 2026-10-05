package postgres

import (
	"context"
	"fmt"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// BackfillDetail copies ledger rows the mirror is missing.
//
// It walks a cursor rather than the table, for two reasons. A backfill that
// re-read the whole ledger on every start would be a full scan of the
// authoritative table each time the gateway restarts, on the one datastore that
// must never be slow. And a cursor makes the work resumable: a fleet that has
// been running for a year should copy the year once, not once per restart.
//
// The cursor is compared against the *maximum* id, so rows inserted while the
// backfill is running are picked up by a later pass rather than being missed by
// this one. That is why this does not run on a timer: the gateway's own queue
// carries what is being written now, and this covers the past.
//
// Nothing here is authoritative. Every row it writes already exists in
// usage_events, which is why a failure is a gap in reporting rather than a gap
// in the books.
func BackfillDetail(ctx context.Context, db *DB, sink detail.Sink) (int, error) {
	if db == nil || sink == nil {
		return 0, nil
	}
	from, err := db.detailCursor(ctx)
	if err != nil {
		return 0, err
	}
	if err := setDetailCursor(ctx, db, from); err != nil {
		return 0, err
	}

	// Stop at the highest id that existed when the walk started. Reading the
	// ceiling first means a request served mid-backfill is written by the live
	// queue and not also picked up here -- which is harmless but wasteful, and
	// wastefulness in a backfill becomes load.
	var ceiling int64
	if err := db.pool.QueryRow(ctx,
		`SELECT COALESCE(max(id), 0) FROM usage_events WHERE id > $1`, from).Scan(&ceiling); err != nil {
		return 0, fmt.Errorf("postgres: detail backfill ceiling: %w", err)
	}
	if ceiling == from {
		return 0, nil
	}

	copied := 0
	for cursor := from; cursor < ceiling; {
		batch, next, err := db.detailPage(ctx, cursor, ceiling, detailPageSize)
		if err != nil {
			return copied, err
		}
		if len(batch) == 0 {
			// A gap in the id sequence between the cursor and the ceiling, which
			// happens after a rollback. Advancing is correct: the rows are not
			// coming back.
			if err := setDetailCursor(ctx, db, next); err != nil {
				return copied, err
			}
			cursor = next
			continue
		}
		for _, r := range batch {
			sink.Enqueue(r)
		}
		copied += len(batch)
		if err := setDetailCursor(ctx, db, next); err != nil {
			return copied, err
		}
		cursor = next
	}
	return copied, nil
}

// detailPageSize bounds one page. Small enough that a long backfill makes
// visible progress, large enough that the per-query cost is not the dominant
// cost.
const detailPageSize = 2000

// detailPage reads records with an id in (cursor, ceiling], up to a limit.
//
// The ceiling is part of the predicate rather than only the loop bound: without
// it, a burst of traffic could keep the loop alive indefinitely and the backfill
// would never finish, because its limit would always be pushed forward by rows
// newer than the last page.
func (db *DB) detailPage(ctx context.Context, cursor, ceiling int64, limit int) ([]billing.Record, int64, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, tenant_id, project_id, key_id, model, endpoint_id, price_book_id,
		       prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
		       amounts_micro, usage_known, usage_source, truncated,
		       ttft_ms, duration_ms, streamed, occurred_at
		FROM usage_events
		WHERE id > $1 AND id <= $2
		ORDER BY id
		LIMIT $3`, cursor, ceiling, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: reading usage for the detail store: %w", err)
	}
	defer rows.Close()

	var out []billing.Record
	var lastID int64
	for rows.Next() {
		r, err := scanDetailRow(rows)
		if err != nil {
			return nil, lastID, err
		}
		out = append(out, r)
		lastID = r.LedgerID
	}
	if err := rows.Err(); err != nil {
		return nil, lastID, err
	}
	// The next cursor is the last row this page read, because the predicate is
	// strictly greater than. Storing one past it would skip exactly one row at
	// every page boundary -- silently, since nothing reports a gap -- so a
	// ten-million-row ledger would lose five thousand usage events and the
	// mirror would disagree with the books by an amount nobody could trace.
	//
	// An empty page means a hole in the sequence, which is what a rollback
	// leaves behind and those rows are never coming back. There is nothing
	// between here and the ceiling, so the walk is done.
	if len(out) == 0 {
		lastID = ceiling
	}
	return out, lastID, nil
}

func (db *DB) detailCursor(ctx context.Context) (int64, error) {
	var id int64
	err := db.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT value FROM detail_state WHERE key = 'cursor'), '0')::bigint`).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("postgres: reading the detail cursor: %w", err)
	}
	return id, nil
}

func setDetailCursor(ctx context.Context, db *DB, id int64) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO detail_state (key, value, updated_at)
		VALUES ('cursor', $1, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		fmt.Sprint(id))
	if err != nil {
		return fmt.Errorf("postgres: advancing the detail cursor: %w", err)
	}
	return nil
}

// DetailCursor reports how far the backfill has reached.
//
// A zero cursor is a legitimate position -- a database with no usage yet -- and
// it is also what a database that has never been mirrored reports. Callers that
// need to tell those apart read detail_state directly.
func (db *DB) DetailCursor(ctx context.Context) (int64, error) {
	return db.detailCursor(ctx)
}
