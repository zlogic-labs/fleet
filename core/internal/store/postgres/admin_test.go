package postgres

import (
	"context"
	"testing"
)

// Deleting a tenant or a project that already has a ledger is refused, because
// the ledger is the only record of what was charged and the row it would leave
// behind points at a scope nobody can name any more.
//
// The refusal is a WHERE count over usage_events, which means it only works if
// the ledger records the same project id that the API deletes by. For a while
// it did not — the gateway wrote the bare project name while this counted
// "tenant/name" — so every delete found zero rows and every delete succeeded.

// adminFixture is a tenant with one project and no usage yet.
func adminFixture(t *testing.T, db *DB) {
	t.Helper()
	truncate(t, db, "usage_events", "api_keys", "projects", "tenants")
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme')`)
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name) VALUES ('acme/research','acme','research')`)
}

// charge writes one ledger row against a project, the way the gateway does.
func charge(t *testing.T, db *DB, tenant, project string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO usage_events
		(tenant_id, project_id, model, endpoint_id, amounts_micro, occurred_at)
		VALUES ($1, $2, 'demo/model', 'demo', 1000, now())`, tenant, project)
}

func TestDeletingAProjectWithALedgerIsRefused(t *testing.T) {
	db := testDB(t)
	adminFixture(t, db)
	ctx := context.Background()

	// Nothing spent yet: this is the ordinary case, and it has to keep working.
	if err := db.DeleteProject(ctx, "acme/research"); err != nil {
		t.Fatalf("delete an unused project: %v", err)
	}
}

func TestDeletingAProjectThatWasChargedIsRefused(t *testing.T) {
	db := testDB(t)
	adminFixture(t, db)
	ctx := context.Background()
	charge(t, db, "acme", "acme/research")

	err := db.DeleteProject(ctx, "acme/research")
	if !Conflict(err) {
		t.Fatalf("deleting a charged project returned %v, want a conflict", err)
	}
}

func TestDeletingATenantThatWasChargedIsRefused(t *testing.T) {
	db := testDB(t)
	adminFixture(t, db)
	ctx := context.Background()
	charge(t, db, "acme", "acme/research")

	if err := db.DeleteTenant(ctx, "acme"); !Conflict(err) {
		t.Fatalf("deleting a charged tenant returned %v, want a conflict", err)
	}
}

func TestAProjectOthersChargedMayBeDeleted(t *testing.T) {
	// The refusal is about this project's ledger, not about the tenant having
	// one. Otherwise the first charge would lock a tenant out of deleting any
	// project forever.
	db := testDB(t)
	adminFixture(t, db)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name) VALUES ('acme/other','acme','other')`)
	charge(t, db, "acme", "acme/research")

	if err := db.DeleteProject(ctx, "acme/other"); err != nil {
		t.Fatalf("deleting an uncharged project beside a charged one: %v", err)
	}
}
