package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Tenant struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	RequestLimit  int64     `json:"requestLimit"`
	TokenLimit    int64     `json:"tokenLimit"`
	Active        bool      `json:"active"`
	CreatedAtUnix int64     `json:"createdAt"`
	Projects      []Project `json:"projects"`
}

type Project struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenantId"`
	Name          string `json:"name"`
	RequestLimit  int64  `json:"requestLimit"`
	TokenLimit    int64  `json:"tokenLimit"`
	CreatedAtUnix int64  `json:"createdAt"`
}

type Key struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenantId"`
	ProjectID     string `json:"projectId"`
	Label         string `json:"label"`
	Prefix        string `json:"prefix"`
	CreatedAtUnix int64  `json:"createdAt"`
	RevokedAtUnix *int64 `json:"revokedAt,omitempty"`
}

const tenantCols = `id, name, request_limit, token_limit, active, extract(epoch from created_at)::bigint`

func (d *DB) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.RequestLimit, &t.TokenLimit,
			&t.Active, &t.CreatedAtUnix); err != nil {
			return nil, fmt.Errorf("postgres: scan tenant: %w", err)
		}
		t.Projects = []Project{}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := d.pool.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.RequestLimit, &t.TokenLimit, &t.Active, &t.CreatedAtUnix)
	if err != nil {
		return Tenant{}, wrapNotFound(err, "tenant %s", id)
	}
	t.Projects, err = d.ListProjects(ctx, id)
	if err != nil {
		return Tenant{}, err
	}
	if t.Projects == nil {
		t.Projects = []Project{}
	}
	return t, nil
}

func (d *DB) UpdateTenant(ctx context.Context, id, name string, req, tok int64, active bool) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE tenants SET name = $2, request_limit = $3, token_limit = $4, active = $5
		  WHERE id = $1`, id, name, req, tok, active)
	if err != nil {
		return fmt.Errorf("postgres: update tenant %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("tenant %s", id)
	}
	return nil
}

func (d *DB) DeleteTenant(ctx context.Context, id string) error {
	return d.deleteCascade(ctx, "tenant", id,
		`DELETE FROM tenants WHERE id = $1`)
}

func (d *DB) DeleteProject(ctx context.Context, id string) error {
	return d.deleteCascade(ctx, "project", id,
		`DELETE FROM projects WHERE id = $1`)
}

func (d *DB) ListProjects(ctx context.Context, tenantID string) ([]Project, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, tenant_id, name, request_limit, token_limit,
		        extract(epoch from created_at)::bigint
		   FROM projects WHERE ($1 = '' OR tenant_id = $1) ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list projects: %w", err)
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.RequestLimit,
			&p.TokenLimit, &p.CreatedAtUnix); err != nil {
			return nil, fmt.Errorf("postgres: scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetProject(ctx context.Context, id string) (Project, error) {
	var p Project
	err := d.pool.QueryRow(ctx,
		`SELECT id, tenant_id, name, request_limit, token_limit,
		        extract(epoch from created_at)::bigint
		   FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.TenantID, &p.Name, &p.RequestLimit, &p.TokenLimit, &p.CreatedAtUnix)
	if err != nil {
		return Project{}, wrapNotFound(err, "project %s", id)
	}
	return p, nil
}

func (d *DB) UpdateProject(ctx context.Context, id, name string, req, tok int64) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE projects SET name = $2, request_limit = $3, token_limit = $4 WHERE id = $1`,
		id, name, req, tok)
	if err != nil {
		return fmt.Errorf("postgres: update project %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("project %s", id)
	}
	return nil
}

// deleteCascade refuses when anything was billed under the scope.
//
// Tenants and projects are not deletable once they have usage: the ledger
// refers to them, and a DELETE would either fail on the foreign key or succeed
// after the fact if the reference were ever dropped. Disabling is the honest
// answer, so active=false exists on the tenant and nothing here pretends a
// costing relationship can be un-billed.
func (d *DB) deleteCascade(ctx context.Context, kind, id, stmt string) error {
	return d.inTx(ctx, func(tx pgx.Tx) error {
		var used int64
		q := `SELECT count(*) FROM usage_events WHERE ` + map[string]string{
			"tenant": "tenant_id", "project": "project_id",
		}[kind] + ` = $1`
		if err := tx.QueryRow(ctx, q, id).Scan(&used); err != nil {
			return fmt.Errorf("postgres: count usage for %s %s: %w", kind, id, err)
		}
		if used > 0 {
			// conflict, not a fault: the resource exists, and its state is what
			// the request cannot proceed past. Returning a bare error here made
			// the refusal a 500, which tells the caller the platform is broken
			// when in fact it is working exactly as intended.
			return fmt.Errorf("%w: %s %s has %d billed requests; set it inactive instead",
				conflict, kind, id, used)
		}
		if _, err := tx.Exec(ctx, stmt, id); err != nil {
			return fmt.Errorf("postgres: delete %s %s: %w", kind, id, err)
		}
		return nil
	})
}
