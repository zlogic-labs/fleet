package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail"
	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
)

// detailSink opens the reporting replica, if one is configured.
//
// No URL means no store, and no store is not a degraded mode: detail.Nop accepts
// every record and does nothing, which is the same shape with nothing behind it.
// That is why this returns no error for the empty case -- a gateway with no
// ClickHouse is a complete gateway.
//
// An unreachable ClickHouse is a startup failure, unlike an unreachable replica
// mid-flight. The difference is what the operator would otherwise see: a gateway
// that starts and silently stops mirroring is a gateway whose reports quietly
// rot, and nothing in the product says so.
func detailSink(ctx context.Context, cfg Config, log *slog.Logger) (detail.Sink, func(context.Context) error, error) {
	url := cfg.Detail.URL
	if url == "" {
		log.Info("no detail store configured; reports will read postgres directly")
		return detail.Nop{}, nil, nil
	}

	store, err := clickhouse.Open(ctx, cfg.Detail)
	if err != nil {
		return nil, nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, nil, err
	}

	var dropped int64
	queue := detail.NewQueue(clickhouse.NewWriter(store), log, func(n int) {
		dropped += int64(n)
		if dropped%1000 < int64(n) {
			// Every thousandth, not every record: a store that is down would
			// otherwise fill the log with one line per request and turn a
			// reporting problem into an outage of the serving process's disk.
			log.Warn("the detail store is not keeping up; its figures can be rebuilt with the backfill",
				"dropped_since_start", dropped)
		}
	})
	log.Info("usage will be mirrored to the detail store",
		"database", cfg.Detail.Database, "url", url)

	return queue, func(context.Context) error { return queue.Close() }, nil
}

// startBackfill copies the ledger into the detail store in the background.
//
// It exists because the mirror cannot be trusted to be complete: its queue drops
// on overflow, its writes are not retried, and the store may be restored from an
// older backup. All three are acceptable precisely because the ledger can supply
// the rows again.
//
// Run once at startup rather than on a timer, because a fleet that is missing
// historical rows is missing them in the past too, and a periodic scan would be
// re-reading a range it has already copied. The cursor is stored in Postgres, so
// a restart resumes rather than starting over.
func startBackfill(ctx context.Context, db *sqlstore.DB, sink detail.Sink, log *slog.Logger) {
	if db == nil {
		return
	}
	go func() {
		copied, err := sqlstore.BackfillDetail(ctx, db, sink)
		if err != nil {
			log.Warn("the detail backfill did not finish; reports may be missing recent history",
				"copied", copied, "err", err)
			return
		}
		if copied > 0 {
			log.Info("backfilled the ledger into the detail store", "records", copied)
		}
	}()
}

// detailTimeout bounds opening the store at startup.
const detailTimeout = 15 * time.Second
