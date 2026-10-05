package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// The rate limiter, against rate_counters.
//
// It exists because the in-process limiter is per-process, and a gateway
// running N replicas with a per-process counter enforces N times the limit it
// declares. Nothing breaks when that happens: the counter is not wrong about
// anything it was asked, each replica admits exactly its own share, and the sum
// over the replicas is quietly off by the replica count. A tenant that bought
// 100 requests a minute gets 100 per pod.
//
// The budget already solves the same problem and the shape is deliberately the
// same one: a lock on the tenant row for the whole check, and a sum over time
// buckets rather than a running total. The counters do not go in
// spend_counters, because a budget rule's window would then sum over the rate
// limiter's one-second rows and count them twice.

// RateLimiter enforces ratelimit.Limiter against PostgreSQL.
type RateLimiter struct {
	db         *DB
	PolicyFor  func(ratelimit.Scope) ratelimit.Policies
	Window     time.Duration
	now        func() time.Time
	seq        atomic.Uint64
	Resolution time.Duration
}

// NewRateLimiter returns a limiter sharing state with every replica of the same
// gateway.
func NewRateLimiter(db *DB, policyFor func(ratelimit.Scope) ratelimit.Policies) *RateLimiter {
	return &RateLimiter{
		db:         db,
		PolicyFor:  policyFor,
		Window:     ratelimit.DefaultWindow,
		Resolution: time.Second,
		now:        time.Now,
	}
}

var _ ratelimit.Limiter = (*RateLimiter)(nil)

// Reserve charges both levels or neither.
//
// Both live in one transaction behind one lock on the tenant row. Two
// transactions would each see the other's uncommitted deductions and both
// admit, which is the same check-then-act hole the in-process limiter closes by
// holding a mutex; the database equivalent is a lock, not a faster query.
func (l *RateLimiter) Reserve(ctx context.Context, req ratelimit.Request) (ratelimit.Reservation, error) {
	scope := req.Scope.Normalized()
	policies := l.policies(scope)
	if policies.Unlimited() {
		return ratelimit.Admitted(scope.Tenant, scope.Project), nil
	}

	now := l.now()
	reset := l.windowEnd(now)
	from := now.Add(-l.Window).Unix()

	err := l.db.inTx(ctx, func(tx pgx.Tx) error {
		if err := l.lockTenant(ctx, tx, scope); err != nil {
			return err
		}
		if err := l.admit(ctx, tx, scope.TenantKey(), policies.Envelope,
			scope, ratelimit.KindTenant, req.Tokens, now, from, reset); err != nil {
			return err
		}
		if scope.Project == "" {
			return nil
		}
		// Returning the error rolls the whole transaction back, including the
		// envelope charge above. An explicit refund here would be undone by the
		// rollback anyway, and would read as though the rollback did not exist.
		return l.admit(ctx, tx, scope.Key(), policies.Partition,
			scope, ratelimit.KindProject, req.Tokens, now, from, reset)
	})
	if err != nil {
		return ratelimit.Reservation{}, err
	}

	id := fmt.Sprintf("rsv-%d-%s", l.seq.Add(1), scope)
	return ratelimit.NewReservation(id, scope.Tenant, scope.Project, req.Tokens, now), nil
}

// lockTenant serialises every scope belonging to one tenant against the others.
//
// The tenant row is the target because it always exists. A table of lock rows
// would need a row created by the first request, which is a write outside the
// transaction that needs it.
func (l *RateLimiter) lockTenant(ctx context.Context, tx pgx.Tx, scope ratelimit.Scope) error {
	var id string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, scope.Tenant).Scan(&id); err != nil {
		return fmt.Errorf("postgres: lock tenant %s for rate limiting: %w", scope.Tenant, err)
	}
	return nil
}

// admit checks one level and, if it fits, charges it.
//
// The token ceiling reads reserved PLUS settled. Settling moves a request's
// tokens out of the reservation and into the settled total, so a check reading
// only reserved would see the window drain after every request completed — a
// steady stream of short requests would never reach a ceiling at all.
func (l *RateLimiter) admit(ctx context.Context, tx pgx.Tx, key string, policy ratelimit.Policy,
	scope ratelimit.Scope, kind ratelimit.Kind, tokens int,
	now time.Time, from int64, reset time.Duration) error {

	var requests, reserved, settled int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(reserved), 0), COALESCE(SUM(settled), 0)
		  FROM rate_counters
		 WHERE scope = $1 AND second > $2`, key, from).Scan(&requests, &reserved, &settled)
	if err != nil {
		return fmt.Errorf("postgres: sum the window for %s: %w", key, err)
	}

	if policy.RequestsPerMinute > 0 && requests >= int64(policy.RequestsPerMinute) {
		return ratelimit.LimitedError(scope, kind, "request rate",
			int(requests), policy.RequestsPerMinute, reset)
	}
	if policy.TokensPerMinute > 0 && reserved+settled+int64(tokens) > int64(policy.TokensPerMinute) {
		return ratelimit.LimitedError(scope, kind, "token rate",
			int(reserved+settled), policy.TokensPerMinute, reset)
	}

	const stmt = `
		INSERT INTO rate_counters (scope, second, requests, reserved, inflight)
		VALUES ($1, $2, 1, $3, 1)
		ON CONFLICT (scope, second) DO UPDATE
		   SET requests = rate_counters.requests + 1,
		       reserved = rate_counters.reserved + $3,
		       inflight = rate_counters.inflight + 1`
	if _, err := tx.Exec(ctx, stmt, key, now.Unix(), tokens); err != nil {
		return fmt.Errorf("postgres: charge the window for %s: %w", key, err)
	}
	return nil
}

// policies fetches the limits in force, with negatives read as unlimited.
//
// A negative limit reads as "no limit" in a config file, which is a limit
// nobody wrote on purpose. Handled here rather than in the parser because a
// stored policy can arrive from either source.
func (l *RateLimiter) policies(scope ratelimit.Scope) ratelimit.Policies {
	p := ratelimit.Policies{}
	if l.PolicyFor != nil {
		p = l.PolicyFor(scope)
	}
	p.Envelope.RequestsPerMinute = positive(p.Envelope.RequestsPerMinute)
	p.Envelope.TokensPerMinute = positive(p.Envelope.TokensPerMinute)
	p.Partition.RequestsPerMinute = positive(p.Partition.RequestsPerMinute)
	p.Partition.TokensPerMinute = positive(p.Partition.TokensPerMinute)
	return p
}

func positive(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// windowEnd is when the window rolls over, which is what a 429 tells a client.
func (l *RateLimiter) windowEnd(now time.Time) time.Duration {
	return now.Truncate(l.Resolution).Add(l.Window).Sub(now)
}
