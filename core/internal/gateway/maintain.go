package gateway

import (
	"context"
	"log/slog"
	"time"

	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
)

// Counter housekeeping.
//
// Both limiters accumulate time buckets, and neither deletes one on its own: a
// row is invisible to every check the moment it slides out of the window, but
// it is still a row. Left alone, a busy tenant writes 86 400 of them a day and
// the table grows for as long as the gateway runs.
//
// The deletion is scheduled here rather than run inline because it is pure
// housekeeping: every row it removes was already unreadable. Running it on the
// request path would put a table-wide scan in front of somebody's completion.

// pruneEvery is how often the counters are swept. Short enough that a crashed
// gateway does not leave much behind, long enough that the delete is a rounding
// error against the writes it accompanies.
const pruneEvery = 5 * time.Minute

// Pruner is anything holding time buckets that outlive their usefulness.
type Pruner interface {
	Prune(context.Context) (int64, error)
}

// startPruner runs until the context is cancelled.
//
// Failure is logged and retried on the next tick rather than fatal: a database
// that is briefly unreachable leaves counters unreadable, not wrong, and a
// gateway that exits because it could not delete old rows would turn a
// housekeeping problem into an outage.
func startPruner(ctx context.Context, log *slog.Logger, pruners ...Pruner) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, p := range pruners {
				cut, err := p.Prune(ctx)
				if err != nil {
					log.Warn("counter prune failed", "err", err)
					continue
				}
				if cut > 0 {
					log.Debug("counter buckets pruned", "rows", cut)
				}
			}
		}
	}
}

// prunersFor builds the sweep for a gateway with a database.
//
// With no database there is nothing to sweep: the in-process limiter keeps its
// counters in a fixed-size ring that recycles its own slots.
func prunersFor(db *sqlstore.DB) []Pruner {
	if db == nil {
		return nil
	}
	return []Pruner{sqlstore.NewRateLimiter(db, nil), sqlstore.NewQuota(db)}
}
