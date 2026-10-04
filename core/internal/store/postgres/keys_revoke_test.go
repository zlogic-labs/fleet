package postgres

import (
	"context"
	"testing"
	"time"
)

// The Postgres key store is the only implementation an operator uses, and both
// bugs these cover lived here while the in-memory one was correct — so nothing
// comparing the two would ever have found them. Every test here needs the real
// database; there is no fixture-free version worth having.

func keyFixture(t *testing.T, db *DB) (secret, keyID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ('revoke-probe', 'Probe')
		 ON CONFLICT (id) DO UPDATE SET active = true`); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO projects (id, tenant_id, name) VALUES ('revoke-probe/p','revoke-probe', 'p')
		 ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("project: %v", err)
	}
	secret, keyID, err := NewKeyStore(db, nil).CreateKey(ctx, "revoke-probe", "p", "probe")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return secret, keyID
}

// Deleting a key has to stop it working. This is the response to a leaked
// credential, so a delete that returns 204 and leaves the key usable is worse
// than no delete at all: the operator's incident notes say it is handled.
func TestARevokedKeyStopsAuthenticating(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()
	secret, keyID := keyFixture(t, db)

	store := NewKeyStore(db, NewTTLCache(time.Minute, 64))
	if _, ok := store.Lookup(ctx, secret); !ok {
		t.Fatal("a fresh key does not authenticate")
	}

	if err := db.DeleteKey(ctx, keyID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	// A fresh store, because the lookup is cached for five minutes in
	// production. A cache that outlives a revocation is a second way for this
	// bug to survive, and testing the cached path is how that gets caught.
	after := NewKeyStore(db, NewTTLCache(time.Minute, 64))
	if _, ok := after.Lookup(ctx, secret); ok {
		t.Error("a revoked key still authenticates; deleting a leaked credential does nothing")
	}

	// And the row is still there — revoked, not deleted. An operator who
	// revokes by accident needs the audit trail and a way back.
	var revokedAt *time.Time
	if err := db.pool.QueryRow(ctx,
		`SELECT revoked_at FROM api_keys WHERE id = $1`, keyID).Scan(&revokedAt); err != nil {
		t.Fatalf("the row is gone rather than revoked: %v", err)
	}
	if revokedAt == nil {
		t.Error("revoked_at is null after a delete; the key is unusable but indistinguishable from a live one")
	}
}

// The expiry test has to write expires_at by hand because nothing in the
// product does yet, which is exactly why the inverted comparison survived: the
// branch had never run with a non-nil column.
func TestExpiryIsReadTheRightWayRound(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	live, liveID := keyFixture(t, db)
	expired, expiredID := keyFixture(t, db)
	later, laterID := keyFixture(t, db)

	if _, err := db.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() + interval '1 day' WHERE id = $1`, liveID); err != nil {
		t.Fatalf("set a future expiry: %v", err)
	}
	if _, err := db.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 day' WHERE id = $1`, expiredID); err != nil {
		t.Fatalf("set a past expiry: %v", err)
	}
	if _, err := db.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() + interval '1 hour' WHERE id = $1`, laterID); err != nil {
		t.Fatalf("set a near expiry: %v", err)
	}

	store := NewKeyStore(db, NewTTLCache(time.Minute, 64))

	// The two that used to come out backwards: a key valid for a day was
	// refused, and a key expired for a day was accepted.
	if _, ok := store.Lookup(ctx, live); !ok {
		t.Error("a key valid for another day was refused; the comparison is inverted")
	}
	if _, ok := store.Lookup(ctx, expired); ok {
		t.Error("a key that expired yesterday was accepted; the comparison is inverted")
	}
	// The one that was never wrong and must stay right: a key expiring in an
	// hour is still usable now.
	if _, ok := store.Lookup(ctx, later); !ok {
		t.Error("a key expiring in an hour was refused")
	}
}

// No expiry set is the state every key is in today, and it must mean "no
// expiry" rather than "already expired".
func TestAKeyWithNoExpiryNeverExpires(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	secret, _ := keyFixture(t, db)
	store := NewKeyStore(db, NewTTLCache(time.Minute, 64))
	if _, ok := store.Lookup(context.Background(), secret); !ok {
		t.Error("a key with no expiry was refused")
	}
}

// Deactivating the tenant is the other half of cutting access, and it lives in
// the same query.
func TestADeactivatedTenantLosesItsKeys(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()
	secret, _ := keyFixture(t, db)
	store := NewKeyStore(db, NewTTLCache(time.Minute, 64))
	if _, ok := store.Lookup(ctx, secret); !ok {
		t.Fatal("premise: the key does not work before the tenant is switched off")
	}
	mustExec(t, db, `UPDATE tenants SET active = false WHERE id = 'revoke-probe'`)

	after := NewKeyStore(db, NewTTLCache(time.Minute, 64))
	if _, ok := after.Lookup(ctx, secret); ok {
		t.Error("a key of a deactivated tenant still authenticates")
	}
}
