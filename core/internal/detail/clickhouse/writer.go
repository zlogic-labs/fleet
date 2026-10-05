package clickhouse

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// insertBatch bounds one INSERT.
//
// ClickHouse handles a batch of a few thousand rows far better than a thousand
// small ones: each one is a round trip and its own part file. It is not unbounded
// because a batch that is too large is held in memory by both ends at once, and
// this runs in the same process as request serving.
const insertBatch = 2000

// Writer implements detail.Writer.
type Writer struct {
	store *Store
	// batch is reused across writes so a steady stream does not allocate one
	// slice per flush. The driver copies what it needs before PrepareBatch
	// returns, so holding onto it is safe.
	batch []driver.Batch
}

// NewWriter returns a writer bound to a store.
func NewWriter(s *Store) *Writer { return &Writer{store: s} }

// Write inserts a batch of settled records.
//
// Records are written in chunks of insertBatch rather than in one statement,
// because a single INSERT that grows without bound is how a detail store takes
// down the process that feeds it.
func (w *Writer) Write(ctx context.Context, records []billing.Record) error {
	for start := 0; start < len(records); start += insertBatch {
		end := min(start+insertBatch, len(records))
		if err := w.writeChunk(ctx, records[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeChunk(ctx context.Context, records []billing.Record) error {
	b, err := w.store.conn.PrepareBatch(ctx, "INSERT INTO "+w.store.db+".usage_detail")
	if err != nil {
		return err
	}
	for _, r := range records {
		if err := appendRecord(b, r); err != nil {
			// A column the driver and the table disagree about is a bug that
			// will fire again on every flush, so it is returned rather than
			// dropped one record at a time.
			return err
		}
	}
	return b.Send()
}

func appendRecord(b driver.Batch, r billing.Record) error {
	occurred := r.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	return b.Append(
		r.LedgerID,
		occurred,
		r.Tenant,
		r.Project,
		r.KeyID,
		r.Model,
		r.Endpoint,
		r.PriceBook,
		r.Usage.PromptTokens,
		r.Usage.CompletionTokens,
		detailBreakdown(r.Usage.CachedPromptTokens(), r.Usage.PromptTokensDetails != nil),
		detailBreakdown(r.Usage.ReasoningTokens(), r.Usage.CompletionTokensDetails != nil),
		int64(r.Amount),
		string(sourceOrEngine(r.UsageSource)),
		r.UsageKnown,
		r.Truncated,
		r.TTFT.Milliseconds(),
		r.Duration.Milliseconds(),
		r.Streamed,
		engineTiming(r.Engine, func(e *billing.EngineTimings) *float64 { return e.QueueMS }),
		engineTiming(r.Engine, func(e *billing.EngineTimings) *float64 { return e.TTFTMS }),
		engineTiming(r.Engine, func(e *billing.EngineTimings) *float64 { return e.DecodeMS }),
	)
}

// engineTiming reads one of an engine's own timing figures, or NULL.
//
// Deliberately not shared with the ledger's identically-named helper. The two
// stores are separate modules and this one is a copy that is allowed to be
// dropped and rebuilt from the ledger, so a shared helper would be a shared
// dependency on the one thing the mirror is not.
func engineTiming(e *billing.EngineTimings, pick func(*billing.EngineTimings) *float64) *float64 {
	if e == nil {
		return nil
	}
	return pick(e)
}

// Close satisfies detail.Writer. There is nothing buffered here, because the
// driver sends on Send, so this only exists for the interface.
func (w *Writer) Close() error { return nil }

// sourceOrEngine maps the ledger's spelling onto the column's Enum.
//
// The default matters: a source the table does not know would fail the whole
// batch, and "engine" is both the most common and the least surprising fallback
// for a record that predates the column.
func sourceOrEngine(s billing.Source) billing.Source {
	switch s {
	case billing.SourceEngine, billing.SourceCounted, billing.SourceReserved:
		return s
	default:
		return billing.SourceEngine
	}
}
