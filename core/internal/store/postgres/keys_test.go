package postgres

import (
	"context"
	"sync"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/authn"
)

// ── keys ───────────────────────────────────────────────────────

// A key resolves to the tenant and project it was created under, and nothing
// else. This is the whole of authentication against a database.
func TestKeyResolvesToItsTenantAndProject(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	keys := NewKeyStore(db, nil)
	mustCreate(t, db, keys, ctx, "acme", "research", "team laptop")

	key, _, err := keys.CreateKey(ctx, "acme", "research", "second")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	p, ok := keys.Lookup(ctx, key)
	if !ok {
		t.Fatal("a key that was just created did not resolve")
	}
	if p.Tenant != "acme" || p.Project != "research" {
		t.Errorf("principal = %s/%s, want acme/research", p.Tenant, p.Project)
	}
	if p.KeyID == "" {
		t.Error("principal carries no key id, so one key cannot be revoked without revoking all")
	}

	if _, ok := keys.Lookup(ctx, "sk-fleet-not-a-real-key"); ok {
		t.Error("an unknown key resolved to a principal")
	}
}

// Two tenants may name a key the same thing. This is the bug that storing the
// bare key id caused in the in-memory store, and the schema's key_hash UNIQUE
// is what prevents it here.
func TestTwoTenantsMayLabelKeysIdentically(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	keys := NewKeyStore(db, nil)
	mustCreate(t, db, keys, ctx, "acme", "research", "admin")
	mustCreate(t, db, keys, ctx, "globex", "sales", "admin")

	acmeKey, _, err := keys.CreateKey(ctx, "acme", "research", "admin")
	if err != nil {
		t.Fatalf("acme key: %v", err)
	}
	globexKey, _, err := keys.CreateKey(ctx, "globex", "sales", "admin")
	if err != nil {
		t.Fatalf("globex key: %v", err)
	}
	if acmeKey == globexKey {
		t.Fatal("two tenants were issued the same key")
	}

	// The bug: acme's key resolving to globex, because one registration
	// overwrote the other under a shared name.
	if p, _ := keys.Lookup(ctx, acmeKey); p.Tenant != "acme" {
		t.Errorf("acme's key resolved to tenant %q — a credential crossed tenants", p.Tenant)
	}
	if p, _ := keys.Lookup(ctx, globexKey); p.Tenant != "globex" {
		t.Errorf("globex's key resolved to tenant %q", p.Tenant)
	}
}

// The stored column is a hash, so the table cannot be dumped into a support
// ticket and turned into working credentials.
func TestKeysAreStoredHashed(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	keys := NewKeyStore(db, nil)
	mustCreate(t, db, keys, ctx, "acme", "research", "laptop")
	key, _, err := keys.CreateKey(ctx, "acme", "research", "laptop")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	var stored []byte
	if err := db.pool.QueryRow(ctx, `SELECT key_hash FROM api_keys`).Scan(&stored); err != nil {
		t.Fatalf("read key_hash: %v", err)
	}
	if string(stored) == key {
		t.Fatal("the key is stored in plaintext")
	}
	if len(stored) != 32 {
		t.Errorf("key_hash is %d bytes, want 32", len(stored))
	}
}

// Three lookups of the same key cost one database round trip. A gateway that
// queried Postgres on every request would put the tenant's database on the hot
// path of every completion, which is a latency and an availability decision
// nobody should be making by accident.
//
// Misses are what is counted, not gets: a cache is consulted on every lookup by
// definition, and counting consultations would assert nothing.
func TestRepeatedLookupsCostOneQuery(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	cache := &countingCache{}
	keys := NewKeyStore(db, cache)
	mustCreate(t, db, keys, ctx, "acme", "research", "laptop")
	key, _, err := keys.CreateKey(ctx, "acme", "research", "laptop")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, ok := keys.Lookup(ctx, key); !ok {
			t.Fatalf("lookup %d: the key stopped resolving", i)
		}
	}
	if cache.misses() != 1 {
		t.Errorf("the database was consulted %d times for three lookups of one key, want 1",
			cache.misses())
	}
}

// An unknown key must not be cached, or a key created a second later would
// never resolve until the cache expired.
func TestUnknownKeysAreNotCached(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "api_keys", "projects", "tenants")
	ctx := context.Background()

	cache := &countingCache{}
	keys := NewKeyStore(db, cache)
	mustCreate(t, db, keys, ctx, "acme", "research", "laptop")

	if _, ok := keys.Lookup(ctx, "sk-fleet-does-not-exist"); ok {
		t.Fatal("an unknown key resolved")
	}
	created, _, err := keys.CreateKey(ctx, "acme", "research", "second")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, ok := keys.Lookup(ctx, created); !ok {
		t.Error("a key created after an unknown lookup did not resolve — the miss was cached")
	}
}

// mustCreate seeds an active tenant with an unlimited envelope.
//
// A tenant has to exist before a key can be issued into it — CreateKey refuses
// to mint a tenant as a side effect, because a tenant is a billing
// relationship and not something a credential request should be able to create.
func mustCreate(t *testing.T, db *DB, keys *KeyStore, ctx context.Context, tenant, project, label string) {
	t.Helper()
	src := NewPolicySource(db, ratelimit.Policy{})
	if err := src.CreateTenant(ctx, TenantRow{ID: tenant, Name: tenant, Active: true}); err != nil {
		t.Fatalf("seed tenant %s: %v", tenant, err)
	}
	if _, _, err := keys.CreateKey(ctx, tenant, project, label); err != nil {
		t.Fatalf("seed %s/%s: %v", tenant, project, err)
	}
}

// countingCache counts misses, which is the number of database round trips.
type countingCache struct {
	mu     sync.Mutex
	m      map[string]authn.Principal
	missed int
}

func (c *countingCache) Get(key string) (authn.Principal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[key]
	if !ok {
		c.missed++
	}
	return p, ok
}

func (c *countingCache) Put(key string, p authn.Principal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]authn.Principal{}
	}
	c.m[key] = p
}

func (c *countingCache) Drop(string) {}

func (c *countingCache) misses() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missed
}
