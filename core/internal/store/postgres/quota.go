package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// The budget check, against budget_rules and spend_counters.
//
// Two properties of "a rolling window" drive the whole design. It has no
// boundary to reset at, so the spent figure is a sum over time buckets rather
// than a running total. And a sum followed by a deduction is two statements,
// which is a gap for a concurrent request to slip through — so the whole check
// happens under a row lock on the tenant.

// querier is what the sums need: a row-returning query. A *pgxpool.Pool and a
// pgx.Tx both satisfy it, so the same sum serves the request path (inside the
// transaction, under the tenant lock) and the control plane (outside one).
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Quota enforces budget rules from PostgreSQL.
type Quota struct {
	db *DB
}

// NewQuota returns a limiter.
func NewQuota(db *DB) *Quota { return &Quota{db: db} }

// rulesFor returns every rule that applies to a scope: its tenant's, and its
// own if it has any.
//
// A project's rules and its tenant's are enforced together and neither replaces
// the other. They cap different things, and "the tighter of a token rule and a
// money rule" is not a number — the two are not the same resource.
func (q *Quota) rulesFor(ctx context.Context, scope ratelimit.Scope) ([]quota.Rule, error) {
	norm := scope.Normalized()
	if norm.Tenant == "" {
		return nil, nil
	}
	const stmt = `
		SELECT scope_kind, scope_id, dimension, limit_value, window_seconds
		  FROM budget_rules
		 WHERE (scope_kind = 'tenant'  AND scope_id = $1)
		    OR (scope_kind = 'project' AND scope_id = $2)
		 ORDER BY scope_kind, dimension, window_seconds`
	rows, err := q.db.pool.Query(ctx, stmt, norm.Tenant, norm.Tenant+"/"+norm.Project)
	if err != nil {
		return nil, fmt.Errorf("postgres: load budget rules: %w", err)
	}
	defer rows.Close()

	var out []quota.Rule
	for rows.Next() {
		var (
			kind, id, dim  string
			limit, seconds int64
		)
		if err := rows.Scan(&kind, &id, &dim, &limit, &seconds); err != nil {
			return nil, fmt.Errorf("postgres: scan a budget rule: %w", err)
		}
		out = append(out, quota.Rule{
			ScopeKind: quota.ScopeKind(kind),
			ScopeID:   id,
			Dimension: quota.Dimension(dim),
			Limit:     limit,
			Window:    time.Duration(seconds) * time.Second,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate budget rules: %w", err)
	}
	// A stored rule that no longer validates is an operator's mistake. It must
	// not become "no limit" and must not become a refusal nobody can explain.
	for _, r := range out {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("postgres: budget rule %s %s/%s: %w",
				r.ScopeKind, r.ScopeID, r.Dimension, err)
		}
	}
	return out, nil
}

// Reserve commits the estimate against every rule, or refuses.
func (q *Quota) Reserve(ctx context.Context, req quota.Request) (quota.Reservation, error) {

	// A zero Now means the caller has no opinion about the clock. Left alone it
	// would place every counter in a 1970 bucket and compute a reset time in
	// year one, which reaches the client as a Retry-After of nothing — a
	// budget refusal that does not say when to come back.
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	req.Now = now
	scope := req.Scope.Normalized()
	if scope.Tenant == "" {
		return quota.Reservation{}, fmt.Errorf("quota: a budget needs a tenant, not a bare request")
	}
	rules, err := q.rulesFor(ctx, scope)
	if err != nil {
		return quota.Reservation{}, err
	}
	if len(rules) == 0 {
		// No rules is unlimited, and it is the common case: an evaluation, a
		// laptop, every tenant nobody has thought about yet. Nothing is written,
		// so a deployment with no budgets keeps an empty counter table.
		return quota.NewReservation(scope, nil, nil, req.Now), nil
	}

	// One lock for the whole check, not one per rule. Acquiring several in a
	// fixed order still deadlocks two scopes that hold each other's; one lock
	// cannot.
	//
	// It is taken on the transaction below rather than on the pool: a row lock
	// belongs to the session that took it, so locking on a pooled connection
	// and then working in a separate transaction locks a connection nothing
	// else is using, which is worse than no lock at all because it looks like
	// one. The tenant row is the target because it always exists — a table of
	// lock rows would need a row created by the first request, which is a
	// write outside the transaction that needs it.
	var committed []int64
	err = q.db.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, scope.Tenant).Scan(&id); err != nil {
			return fmt.Errorf("postgres: lock tenant %s: %w", scope.Tenant, err)
		}
		committed = make([]int64, len(rules))
		for i, rule := range rules {
			bound := req.Estimate.Measure(rule.Dimension)
			used, err := sum(ctx, tx, rule.ScopeKey(), rule, req.Now)
			if err != nil {
				return err
			}
			if used+bound > rule.Limit {
				// Nothing has been written yet, so refusing here moves no
				// counter. Returning an error also rolls the transaction back,
				// which is what makes "the first rule admitted, the second
				// refused" safe without an explicit undo.
				frees, err := whenFree(ctx, tx, rule.ScopeKey(), rule, req.Now)
				if err != nil {
					return err
				}
				kind := quota.KindTenant
				if rule.ScopeKind == quota.KindProject {
					kind = quota.KindProject
				}
				return &quota.Exceeded{
					Scope: scope, Kind: kind, Dimension: rule.Dimension,
					Used: used, Limit: rule.Limit, Window: rule.Window, ResetsAt: frees,
				}
			}
			committed[i] = bound
		}
		return write(ctx, tx, rules, committed, nil, req.Now, false)
	})
	if err != nil {
		return quota.Reservation{}, err
	}
	return quota.NewReservation(scope, rules, committed, req.Now), nil
}

// sum adds up one rule's dimension over its window.
//
// The column name is interpolated from a closed set, never from request data,
// so there is nothing to escape. PostgreSQL cannot parameterise an identifier,
// which is the only reason this is not a bound value.
