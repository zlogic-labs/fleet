package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The budget check, against spend_counters.
//
// Every reservation is a single conditional UPDATE. That is the whole design:
// the budget is read and the deduction committed by one statement, so two
// concurrent requests cannot both see "under budget" and both spend. A
// SELECT followed by an UPDATE would be a check-then-act limiter wearing a
// budget's clothes — exactly the failure package quota exists to avoid, and one
// that only shows up under concurrency, when there is a customer present.

// Quota reserves and settles budgets from spend_counters.
type Quota struct {
	db  *DB
	now func() time.Time
}

// NewQuota returns a limiter reading budgets from tenants and projects.
func NewQuota(db *DB) *Quota {
	return &Quota{db: db, now: time.Now}
}

// budgets returns the tenant envelope and the project partition for a scope,
// both in micro-units.
//
// A zero limit means unlimited. A missing project budget is therefore also
// unlimited rather than zero: zero would freeze a project that was created
// without one, and a tenant that set envelopes and no partitions would find
// every project unable to spend — the opposite of what an envelope is for.
func (q *Quota) budgets(ctx context.Context, scope ratelimit.Scope) (envelope, partition billing.Amount, err error) {
	var units int64
	if err := q.db.pool.QueryRow(ctx,
		`SELECT budget_units FROM tenants WHERE id = $1`, scope.Tenant).Scan(&units); err != nil {
		return 0, 0, fmt.Errorf("postgres: tenant budget for %s: %w", scope.Tenant, err)
	}
	envelope = billing.Amount(units) * billing.MicroPerUnit

	if scope.Project == "" {
		return envelope, 0, nil
	}
	// By name, not by id, for the same reason PolicySource does: a scope carries
	// the project's short name while its id is the tenant/name path that
	// api_keys.project_id stores. Looking it up by id finds nothing, and a
	// project whose budget cannot be found must not be treated as one with no
	// budget — that reads as unlimited.
	units = 0
	if err := q.db.pool.QueryRow(ctx,
		`SELECT budget_units FROM projects WHERE tenant_id = $1 AND name = $2`,
		scope.Tenant, scope.Project).Scan(&units); err != nil {
		return 0, 0, fmt.Errorf("postgres: project budget for %s/%s: %w",
			scope.Tenant, scope.Project, err)
	}
	return envelope, billing.Amount(units) * billing.MicroPerUnit, nil
}

// Reserve commits the bound against both counters, or refuses it.
//
// Both counters move in one transaction. Admitting the envelope and refusing
// the partition has to roll the envelope back, and a rollback that could fail
// or be forgotten would let a tenant whose own project is out of budget keep
// spending the envelope that project is supposed to be a slice of.
func (q *Quota) Reserve(ctx context.Context, req quota.Request) (quota.Reservation, error) {
	scope := req.Scope.Normalized()
	if scope.Tenant == "" {
		return quota.Reservation{}, fmt.Errorf("quota: a budget needs a tenant, not a bare request")
	}
	envelope, partition, err := q.budgets(ctx, scope)
	if err != nil {
		return quota.Reservation{}, err
	}
	w := quota.CalendarMonth(req.Now)
	tenantKey := ratelimit.Scope{Tenant: scope.Tenant}.Key()
	projectKey := ratelimit.Scope{Tenant: scope.Tenant, Project: scope.Project}.Key()

	err = q.db.inTx(ctx, func(tx pgx.Tx) error {
		if envelope > 0 {
			spent, ok, err := take(ctx, tx, tenantKey, w, req.Bound, envelope)
			if err != nil {
				return err
			}
			if !ok {
				return &quota.Exceeded{
					Scope: ratelimit.Scope{Tenant: scope.Tenant}, Kind: quota.KindEnvelope,
					Spent: spent, Budget: envelope, ResetsAt: w.To,
				}
			}
		}
		if partition > 0 {
			spent, ok, err := take(ctx, tx, projectKey, w, req.Bound, partition)
			if err != nil {
				return err
			}
			if !ok {
				// The envelope was taken a statement ago. It comes back
				// because this function returns an error and inTx rolls the
				// transaction back — not because of an explicit release here,
				// which would be dead code doing an impression of the work.
				//
				// That is worth stating because the alternative is worse than
				// redundant: an explicit release implies the rollback is not
				// there, and a later refactor that starts committing before
				// returning an error would then silently start charging the
				// envelope for traffic the partition refused.
				return &quota.Exceeded{
					Scope: ratelimit.Scope{Tenant: scope.Tenant, Project: scope.Project},
					Kind:  quota.KindPartition,
					Spent: spent, Budget: partition, ResetsAt: w.To,
				}
			}
		}
		return nil
	})
	if err != nil {
		return quota.Reservation{}, err
	}
	return quota.NewReservation(scope, req.Bound, envelope, partition, w), nil
}

// take conditionally adds to a counter, refusing if that would pass the limit.
// It returns the committed figure, so a refusal can say how much is gone.
//
// Both branches of the upsert have to carry the check. A WHERE on the DO UPDATE
// alone guards only the case where the row already exists, so the first
// reservation for any scope would take the INSERT branch unchecked — the
// request that creates the counter is the one request the budget does not apply
// to. The INSERT is therefore a SELECT with a WHERE of its own, and the two
// clauses ask the same question: does this addition fit?
func take(ctx context.Context, tx pgx.Tx, scope string, w quota.Window,
	bound, limit billing.Amount) (spent billing.Amount, ok bool, err error) {

	const q = `
		INSERT INTO spend_counters (scope, window_start, reserved_micro)
		SELECT $1, $2, $3::bigint WHERE $3::bigint <= $4::bigint
		ON CONFLICT (scope, window_start) DO UPDATE
		   SET reserved_micro = spend_counters.reserved_micro + $3
		 WHERE spend_counters.spent_micro + spend_counters.reserved_micro + $3 <= $4
		RETURNING spent_micro + reserved_micro`
	var committed int64
	err = tx.QueryRow(ctx, q, scope, w.From, int64(bound), int64(limit)).Scan(&committed)
	switch {
	case err == nil:
		return billing.Amount(committed), true, nil
	case isNoRows(err):
		// Refused, and the counter was not touched. Read the figure for the
		// message inside the same transaction, so the number quoted to the
		// tenant is the one the decision was made against.
		var gone int64
		if qerr := tx.QueryRow(ctx,
			`SELECT COALESCE(spent_micro,0) + COALESCE(reserved_micro,0)
			   FROM spend_counters WHERE scope = $1 AND window_start = $2`,
			scope, w.From).Scan(&gone); qerr != nil && !isNoRows(qerr) {
			return 0, false, fmt.Errorf("postgres: read %s after a refusal: %w", scope, qerr)
		}
		return billing.Amount(gone), false, nil
	default:
		return 0, false, fmt.Errorf("postgres: reserve %s: %w", scope, err)
	}
}

// Settle replaces a reservation with the real charge.
//
// GREATEST on the release guards a counter that has already been released —
// two settles for one reservation, or a settle that arrives after the window
// rolled over and a rebuild zeroed it. Neither may push the counter negative,
// because a negative reserved_micro silently raises the tenant's available
// budget, and that is the direction of error that pays out money.
//
// A real charge above the reservation is recorded in full rather than capped at
// the bound. The tokens were spent; capping would make the budget disagree with
// the ledger, and reconciliation would then find a bill nobody was charged.
func (q *Quota) Settle(ctx context.Context, r quota.Reservation, actual billing.Amount) {
	w := r.Window()
	release := func(scope string, bound billing.Amount) {
		const stmt = `
			UPDATE spend_counters
			   SET reserved_micro = GREATEST(reserved_micro - $3, 0),
			       spent_micro    = spent_micro + $4
			 WHERE scope = $1 AND window_start = $2`
		if _, err := q.db.pool.Exec(ctx, stmt, scope, w.From, int64(bound), int64(actual)); err != nil {
			// Settlement cannot return an error: the response is already on
			// the wire. A miss here leaves the reservation outstanding, which
			// the window roll or a rebuild clears — the ledger stays the
			// authority either way, and this is the one place a counter
			// being wrong is recoverable.
			_ = err
		}
	}
	if scope := r.Scope(); scope.Tenant != "" {
		release(ratelimit.Scope{Tenant: scope.Tenant}.Key(), r.TenantBound())
	}
	if scope := r.Scope(); scope.Project != "" {
		release(ratelimit.Scope{Tenant: scope.Tenant, Project: scope.Project}.Key(), r.ProjectBound())
	}
}

// Spent reports committed spend for a scope over a window.
func (q *Quota) Spent(ctx context.Context, scope ratelimit.Scope, w quota.Window) (billing.Amount, error) {
	var units int64
	err := q.db.pool.QueryRow(ctx, `
		SELECT COALESCE(spent_micro,0) + COALESCE(reserved_micro,0)
		  FROM spend_counters WHERE scope = $1 AND window_start = $2`,
		scope.Key(), w.From).Scan(&units)
	switch {
	case err == nil:
		return billing.Amount(units), nil
	case isNoRows(err):
		// No row means nothing has been spent and nothing reserved, which is
		// not an error — it is the state of every scope before its first
		// request.
		return 0, nil
	default:
		return 0, fmt.Errorf("postgres: read spend for %s: %w", scope.Key(), err)
	}
}

// isNoRows reports the "the WHERE clause matched nothing" case.
//
// In the budget check that outcome is not an error at all: it is how a refusal
// is spelled. The conditional UPDATE admits a reservation by returning a row and
// refuses it by returning none, which keeps the read and the deduction in one
// statement instead of leaving a SELECT for something to race.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
