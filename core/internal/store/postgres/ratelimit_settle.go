package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// Settlement and reporting for RateLimiter.

// Settle replaces a reservation with the real charge.
//
// It cannot return an error: the response is already on the wire. A miss here
// leaves the reservation outstanding, which Prune clears once the row has slid
// out of the window. The cost pool is built from capacity samples, not from
// these counters, so nothing that ends up on an invoice depends on this
// statement landing.
func (l *RateLimiter) Settle(ctx context.Context, r ratelimit.Reservation, actual int) {
	if !r.WasCounted() {
		return
	}
	now := l.now()
	if err := l.release(ctx, r, actual, now); err != nil {
		// Nothing here is load-bearing after the response. A counter being
		// wrong for one window is recoverable; a log line nobody reads is not
		// worth a failed request path.
		return
	}
}

// release gives the reservation back and records what it actually cost.
//
// The deduction is removed from the row it was charged to, not from whichever
// row holds the oldest tokens: a refund should leave the window at the same
// moment the charge would have, so a caller that mistyped a model name gets its
// minute back at once rather than in sixty seconds.
//
// Settling to zero returns the request count as well. A request that never
// reached an engine consumed no GPU second, and charging it against the
// per-minute request count would let a caller burn a whole allowance on a
// misspelled model.
func (l *RateLimiter) release(ctx context.Context, r ratelimit.Reservation, actual int, now time.Time) error {
	keys := scopeKeys(r)
	requests := 0
	if actual <= 0 {
		requests = 1
	}
	return l.db.inTx(ctx, func(tx pgx.Tx) error {
		const refund = `
			UPDATE rate_counters
			   SET requests = GREATEST(0, requests - $3),
			       reserved = GREATEST(0, reserved - $4),
			       inflight  = GREATEST(0, inflight  - 1)
			 WHERE scope = $1 AND second = $2`
		for _, key := range keys {
			if _, err := tx.Exec(ctx, refund, key, r.Second(), requests, r.Reserved); err != nil {
				return fmt.Errorf("postgres: refund %s: %w", key, err)
			}
		}
		if actual <= 0 {
			return nil
		}
		// Recorded against the current second rather than the second the
		// request arrived in: a long generation spans seconds, and the usage
		// belongs to the moment it was known, which is the moment Retry-After
		// counts from.
		const settle = `
			INSERT INTO rate_counters (scope, second, settled)
			VALUES ($1, $2, $3)
			ON CONFLICT (scope, second) DO UPDATE
			   SET settled = rate_counters.settled + $3`
		for _, key := range keys {
			if _, err := tx.Exec(ctx, settle, key, now.Unix(), actual); err != nil {
				return fmt.Errorf("postgres: settle %s: %w", key, err)
			}
		}
		return nil
	})
}

// scopeKeys is the envelope, plus the partition when one was charged.
func scopeKeys(r ratelimit.Reservation) []string {
	if r.ProjectName() == "" {
		return []string{r.TenantName()}
	}
	return []string{r.TenantName(), r.TenantName() + "/" + r.ProjectName()}
}

// Prune drops rows the window can no longer reach.
//
// Without it the table gains a row per scope per second that had traffic, for
// as long as the gateway runs: a busy tenant is 86 400 rows a day, all of them
// invisible to every check. Nothing older than the window can change an answer,
// so deleting it cannot either.
func (l *RateLimiter) Prune(ctx context.Context) (int64, error) {
	horizon := l.now().Add(-l.Window).Unix()
	tag, err := l.db.pool.Exec(ctx, `DELETE FROM rate_counters WHERE second <= $1`, horizon)
	if err != nil {
		return 0, fmt.Errorf("postgres: prune the rate window: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Snapshot reports every scope that has spent anything in the window.
//
// inflight is summed without the window filter, because a request that is still
// generating has not stopped spending. It drifts low if a reservation settles
// after its row was pruned, which costs a reporting number rather than a limit.
func (l *RateLimiter) Snapshot(ctx context.Context) []ratelimit.Usage {
	from := l.now().Add(-l.Window).Unix()
	rows, err := l.db.pool.Query(ctx, `
		SELECT scope,
		       COALESCE(SUM(requests), 0), COALESCE(SUM(reserved), 0), COALESCE(SUM(settled), 0),
		       COALESCE(SUM(inflight), 0)
		  FROM rate_counters
		 WHERE second > $1
		 GROUP BY scope
		 ORDER BY scope`, from)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []ratelimit.Usage
	for rows.Next() {
		var (
			key                        string
			requests, res, set, flight int64
		)
		if err := rows.Scan(&key, &requests, &res, &set, &flight); err != nil {
			continue
		}
		// The key carries the level in its shape: a bare tenant is an envelope,
		// anything with a slash is a partition inside one.
		scope, kind := ratelimit.Scope{Tenant: key}, ratelimit.KindTenant
		if tenant, project, ok := strings.Cut(key, "/"); ok {
			scope, kind = ratelimit.Scope{Tenant: tenant, Project: project}, ratelimit.KindProject
		}
		out = append(out, ratelimit.Usage{
			Scope: scope, Kind: kind,
			RequestsThisMinute: int(requests),
			TokensReserved:     int(res),
			TokensSettled:      int(set),
			InFlight:           int(flight),
		})
	}
	return out
}
