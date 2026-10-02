package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
)

// Tenant and project storage.
//
// This is where a tenant's rate limits live once the gateway stops reading them
// from a config file. The shape is unchanged — an envelope and a partition —
// because the arithmetic in ratelimit does not care where a policy came from,
// only that both levels arrive.

// Scope is one tenant, or one project inside a tenant.
//
// Deliberately narrower than ratelimit.Scope: this one carries ids for writing
// and knows a tenant has no project, which the limiter's keying type does not
// need to express.
type Scope struct {
	Tenant  string
	Project string
}

// TenantRow is a tenant as stored.
type TenantRow struct {
	ID           string
	Name         string
	RequestLimit int
	TokenLimit   int
	Active       bool
}

// Policy returns the tenant's envelope as a limiter policy.
func (t TenantRow) Policy() ratelimit.Policy {
	return ratelimit.Policy{RequestsPerMinute: t.RequestLimit, TokensPerMinute: t.TokenLimit}
}

// ProjectRow is a project as stored.
type ProjectRow struct {
	ID           string
	TenantID     string
	Name         string
	RequestLimit int
	TokenLimit   int
}

// Policy returns the project's partition as a limiter policy.
func (p ProjectRow) Policy() ratelimit.Policy {
	return ratelimit.Policy{RequestsPerMinute: p.RequestLimit, TokensPerMinute: p.TokenLimit}
}

// PolicySource reads limits for the limiter.
//
// It satisfies the func(Scope) ratelimit.Policies shape the limiter is built
// with, so a gateway backed by Postgres and one backed by FLEET_RATE_TENANTS
// run identical limiter code.
type PolicySource struct {
	db *DB
	// fallback supplies the server defaults for a scope with no row. Kept
	// because "no row" and "no limit" are different answers: an undeclared
	// tenant should run under the deployment's defaults, not unlimited.
	fallback ratelimit.Policy
	// ctx is the source's own context, because the limiter calls this without
	// one: ratelimit's policy callback carries only a scope. It is created
	// here, at construction, and never cancelled — a context cancelled by the
	// constructor's defer would make every later lookup fail, and a failed
	// lookup returns the server defaults, which is to say unlimited.
	ctx context.Context
}

// NewPolicySource returns a source that falls back to def for undeclared scopes.
func NewPolicySource(db *DB, def ratelimit.Policy) *PolicySource {
	return &PolicySource{db: db, fallback: def, ctx: context.Background()}
}

// Policies returns the envelope and the partition for a scope.
//
// Two queries rather than one join because a scope is usually either a tenant
// or a project, never both, and a LEFT JOIN to satisfy both in a single round
// trip would return a row that is mostly NULL.
// The envelope read from the database *replaces* the server default per
// dimension rather than being clamped to it, which is authn.Limits.OrDefaults
// and nothing cleverer. Taking the tighter of the two — the obvious "be safe"
// move — is the bug this once had: a tenant whose database row declares 300
// rpm against a server default of 100 would silently get 100, and an operator
// who raised a limit would have no way to tell it did not take.
//
// A declared limit is a decision. The defaults only fill in dimensions nobody
// declared.
func (s *PolicySource) Policies(scope ratelimit.Scope) ratelimit.Policies {
	out := ratelimit.Policies{Envelope: s.fallback}
	norm := scope.Normalized()
	if norm.Tenant == "" {
		// An anonymous caller is not rationed. See
		// authn.ScopeFromContext for why a shared bucket would be worse.
		return ratelimit.Policies{Envelope: ratelimit.Policy{}}
	}

	var (
		req, tok int
		err      error
	)
	var envReq, envTok int
	// The deadline is per call, not held for the process: the cache means this
	// runs once per scope per process, and a long-lived timer would be a
	// resource kept open for the life of the gateway to govern one query.
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	if norm.Project == "" {
		err = s.db.pool.QueryRow(ctx,
			`SELECT request_limit, token_limit FROM tenants WHERE id = $1 AND active`,
			norm.Tenant).Scan(&envReq, &envTok)
	} else {
		// The partition and its tenant's envelope come back together, because a
		// partition above the envelope can never bind and the operator who wrote
		// it believes they have divided a budget they have not divided. Read
		// separately, the check would race a concurrent envelope change.
		err = s.db.pool.QueryRow(ctx, `
			SELECT p.request_limit, p.token_limit, t.request_limit, t.token_limit
			  FROM projects p JOIN tenants t ON t.id = p.tenant_id
			 WHERE p.tenant_id = $1 AND p.name = $2 AND t.active`,
			norm.Tenant, norm.Project).Scan(&req, &tok, &envReq, &envTok)
	}
	if err != nil {
		// Unknown scope or unreachable database: the server defaults, which are
		// a defined state rather than unlimited-by-accident.
		return out
	}
	out.Envelope = ratelimit.Policy{
		RequestsPerMinute: envReq,
		TokensPerMinute:   envTok,
	}.OrDefaults(s.fallback)
	if norm.Project == "" {
		return out
	}
	// The partition is clamped to the envelope rather than trusted. CreateProject
	// refuses to write a partition that exceeds it, but a row can predate that
	// check or an envelope can be lowered underneath an existing project, and a
	// partition that silently exceeds its envelope is the exact arithmetic error
	// the two-level split exists to prevent.
	out.Partition = ratelimit.Policy{
		RequestsPerMinute: clamp(req, out.Envelope.RequestsPerMinute),
		TokensPerMinute:   clamp(tok, out.Envelope.TokensPerMinute),
	}
	return out
}

// clamp holds a partition to its envelope, leaving "unlimited" alone at either level.
func clamp(partition, envelope int) int {
	if wider(partition, envelope) {
		return envelope
	}
	return partition
}

// CreateTenant inserts a tenant.
func (s *PolicySource) CreateTenant(ctx context.Context, t TenantRow) error {
	const q = `INSERT INTO tenants (id, name, request_limit, token_limit, active)
	           VALUES ($1,$2,$3,$4,$5)`
	_, err := s.db.pool.Exec(ctx, q, t.ID, t.Name, t.RequestLimit, t.TokenLimit, t.Active)
	if err != nil {
		return wrap(err, "create tenant %s", t.ID)
	}
	return nil
}

// CreateProject inserts a project, verifying the partition fits the envelope.
//
// The check lives here rather than in a CHECK constraint because it compares
// two rows in two tables, which SQL CHECK cannot do. Doing it on the write path
// means the invariant holds for every row that exists, whichever code path
// wrote it — a control-plane API, an import, or a migration.
func (s *PolicySource) CreateProject(ctx context.Context, p ProjectRow) error {
	return s.db.inTx(ctx, func(tx pgx.Tx) error {
		var envReq, envTok int
		err := tx.QueryRow(ctx,
			`SELECT request_limit, token_limit FROM tenants WHERE id = $1`, p.TenantID).Scan(&envReq, &envTok)
		if err != nil {
			return fmt.Errorf("postgres: read tenant envelope for %s: %w", p.TenantID, err)
		}
		if wider(p.RequestLimit, envReq) {
			return invalidf("project %s/%s: request limit %d exceeds the tenant envelope %d",
				p.TenantID, p.Name, p.RequestLimit, envReq)
		}
		if wider(p.TokenLimit, envTok) {
			return invalidf("project %s/%s: token limit %d exceeds the tenant envelope %d",
				p.TenantID, p.Name, p.TokenLimit, envTok)
		}
		const q = `INSERT INTO projects (id, tenant_id, name, request_limit, token_limit)
		           VALUES ($1,$2,$3,$4,$5)`
		if _, err := tx.Exec(ctx, q, p.ID, p.TenantID, p.Name, p.RequestLimit, p.TokenLimit); err != nil {
			return wrap(err, "create project %s", p.ID)
		}
		return nil
	})
}

// wider reports whether a partition limit exceeds its envelope.
//
// Both sides must be positive to compare. Zero means unlimited at either
// level, and an unlimited envelope admits any partition — the same rule the
// config path applies, and for the same reason: comparing against an
// undeclared zero would reject every project limit on a tenant that never
// declared an envelope at all.
func wider(partition, envelope int) bool {
	return partition > 0 && envelope > 0 && partition > envelope
}
