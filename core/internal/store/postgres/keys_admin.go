package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// missing marks an absent row. A sentinel rather than a substring, because
// every handler needs to answer "was that a 404" and none of them should be
// matching on text.
var missing = errors.New("not found")

// conflict is a unique-constraint violation. Its own sentinel because "you
// already have one of those" and "we broke" have opposite recovery advice, and
// answering 500 to a retried create is how a client ends up retrying forever.
var conflict = errors.New("already exists")

// invalid is a rule the caller broke. Separate from conflict because the two
// invite opposite responses: fix the request, or use a different one.
var invalid = errors.New("invalid")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", invalid, fmt.Sprintf(format, args...))
}

// Invalid reports whether err is a caller rule violation.
func Invalid(err error) bool { return errors.Is(err, invalid) }

// Conflict reports whether err is a uniqueness violation.
func Conflict(err error) bool { return errors.Is(err, conflict) }

func notFoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", missing, fmt.Sprintf(format, args...))
}

func wrapNotFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf(format, args...)
	}
	return wrap(err, format, args...)
}

// wrap classifies a driver error. 23505 is the unique-constraint violation
// every create can hit; anything else is a fault and stays one.
func wrap(err error, format string, args ...any) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s (%s)", conflict, fmt.Sprintf(format, args...), pgErr.ConstraintName)
	}
	return fmt.Errorf("postgres: "+format+": %w", args...)
}

// NotFound reports whether err is a missing row from this package.
func NotFound(err error) bool { return errors.Is(err, missing) }

func (d *DB) ListKeys(ctx context.Context, projectID string) ([]Key, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, tenant_id, project_id, label, key_prefix,
		        extract(epoch from created_at)::bigint,
		        extract(epoch from revoked_at)::bigint
		   FROM api_keys WHERE ($1 = '' OR project_id = $1) ORDER BY created_at`, projectID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list keys: %w", err)
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.TenantID, &k.ProjectID, &k.Label, &k.Prefix,
			&k.CreatedAtUnix, &k.RevokedAtUnix); err != nil {
			return nil, fmt.Errorf("postgres: scan key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (d *DB) DeleteKey(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
	                              WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("postgres: revoke key %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("key %s", id)
	}
	return nil
}
