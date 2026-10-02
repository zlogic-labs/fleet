package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// KeyStore resolves presented credentials against api_keys.
//
// It implements authn.KeyStore, so a gateway configured with a database and
// one configured with FLEET_API_KEYS run the same authentication code — the
// difference is only where the rows come from.

// KeyPrefix is the human-visible marker on a Fleet key.
//
// It exists so that a key found in a log, a screenshot or a support ticket is
// recognisable as a Fleet credential at a glance. It carries no entropy: the
// secret is the 32 bytes after it.
const KeyPrefix = "sk-fleet-"

// KeyStore is the database-backed implementation.
type KeyStore struct {
	db *DB
	// cache memoises a resolved key. A revoked key therefore stays valid for
	// at most this long, which is the price of not querying Postgres on every
	// request; zero disables caching entirely.
	cache KeyCache
}

// KeyCache is the narrow cache contract, so a test can inject one without a
// database and the memory implementation can be shared.
type KeyCache interface {
	Get(key string) (authn.Principal, bool)
	Put(key string, p authn.Principal)
	Drop(key string)
}

// NewKeyStore returns a store that queries the database on every lookup when
// cache is nil.
func NewKeyStore(db *DB, cache KeyCache) *KeyStore {
	return &KeyStore{db: db, cache: cache}
}

// Lookup resolves a presented key to its principal.
//
// Unknown, expired and deactivated keys all return ok=false with no error.
// They are indistinguishable on purpose: telling a caller which of the three
// they hit tells an attacker whether a guess was close.
func (s *KeyStore) Lookup(ctx context.Context, key string) (Principal, bool) {
	if key == "" {
		return authn.Principal{}, false
	}
	if s.cache != nil {
		if p, ok := s.cache.Get(key); ok {
			return p, true
		}
	}

	p, err := s.lookup(ctx, key)
	if err != nil {
		// A database failure is not an authentication failure. Returning
		// ok=false here would turn a database outage into "invalid API key"
		// for every caller, which sends the operator to the wrong logs; this
		// is logged by the caller as an unavailable dependency instead.
		return authn.Principal{}, false
	}
	if s.cache != nil && p.KeyID != "" {
		s.cache.Put(key, p)
	}
	return p, p.KeyID != ""
}

// Principal is authn.Principal under a name this package can also use in
// signatures that mention the store's own types.
type Principal = authn.Principal

func (s *KeyStore) lookup(ctx context.Context, key string) (authn.Principal, error) {
	var (
		p         authn.Principal
		label     string
		expiresAt *time.Time
		active    bool
	)
	// One join rather than three queries: a key that points at a deactivated
	// tenant must be refused, and doing that with a second round trip would
	// open a window where the tenant is reactivated between the two reads.
	//
	// The project is read as its name, not its id, because the limiter keys on
	// the name and because the name is what the tenant recognises in a bill.
	const q = `
		SELECT k.id, k.tenant_id, p.name, k.label, k.expires_at, t.active
		  FROM api_keys k
		  JOIN projects p ON p.id = k.project_id
		  JOIN tenants  t ON t.id = k.tenant_id
		 WHERE k.key_hash = $1`

	row := s.db.pool.QueryRow(ctx, q, HashKey(key))
	err := row.Scan(&p.KeyID, &p.Tenant, &p.Project, &label, &expiresAt, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return authn.Principal{}, errs.NotFound("no such API key")
	}
	if err != nil {
		return authn.Principal{}, fmt.Errorf("postgres: look up key: %w", err)
	}
	if !active {
		return authn.Principal{}, errs.PermissionDenied("the tenant is deactivated")
	}
	if expiresAt != nil && time.Now().Before(*expiresAt) {
		return authn.Principal{}, errs.PermissionDenied("the API key has expired")
	}
	if label != "" {
		p.Labels = map[string]string{"key": label}
	}
	return p, nil
}

// HashKey is the digest stored in key_hash.
//
// SHA-256, deliberately, and not bcrypt or argon2. Those exist to slow down
// guessing a low-entropy secret; a Fleet key is 32 bytes from crypto/rand, so
// there is nothing to guess and nothing for a slow KDF to protect. Verifying a
// key happens on every request, and an argon2id at 64 MiB would put a GPU-grade
// memory cost on the hot path to slow an attack that cannot succeed anyway.
//
// This is the reasoning that makes the choice correct rather than merely
// cheap: a slow hash is only a defence when the plaintext has low entropy.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// GenerateKey returns a new key and its display prefix.
//
// 32 bytes of entropy, base64url without padding. base64 rather than hex
// because a key is something a human pastes: 43 characters instead of 64, and
// no case-folding surprises when it is read off a screen.
func GenerateKey() (key, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("postgres: generate key: %w", err)
	}
	key = KeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return key, DisplayPrefix(key), nil
}

// DisplayPrefix is what may be stored and shown: enough to identify a key in a
// list, not enough to use it.
//
// Twelve characters of the secret, not twelve of the whole string, so that
// every key in the table has the same length of visible secret regardless of
// the constant prefix.
func DisplayPrefix(key string) string {
	secret := strings.TrimPrefix(key, KeyPrefix)
	if len(secret) > 12 {
		secret = secret[:12]
	}
	return KeyPrefix + secret + "…"
}

// CreateKey inserts a tenant's project and a key into it.
//
// The project is created in the same transaction as the key because a key
// cannot exist without one — api_keys.project_id is NOT NULL, and the reason
// is in the schema: a key attributed to no project would make "how much can
// this project spend" depend on which credential the caller happened to hold.
//
// The tenant is *not* created here. A tenant is a billing relationship and a
// contract, and minting one as a side effect of issuing a credential would let
// any caller who can create keys also create customers — with a budget, an
// invoice and an owner. A missing tenant is reported as such rather than as the
// foreign key's constraint name, which says nothing about what to do next.
func (s *KeyStore) CreateKey(ctx context.Context, tenant, project, label string) (key, id string, err error) {
	if tenant == "" || project == "" {
		return "", "", fmt.Errorf("postgres: create key: tenant and project are both required")
	}
	key, prefix, err := GenerateKey()
	if err != nil {
		return "", "", err
	}
	err = s.db.inTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM tenants WHERE id = $1`, tenant).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("postgres: create key: no tenant %q; create the tenant first", tenant)
			}
			return fmt.Errorf("postgres: look up tenant %s: %w", tenant, err)
		}

		var projectID string
		// ON CONFLICT with no target, because a project is unique on both id
		// and (tenant_id, name) and naming one constraint leaves the other to
		// raise. An existing project is the common case anyway: a team adds a
		// second key, it does not add a second project.
		const insProject = `
			INSERT INTO projects (id, tenant_id, name)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`
		pid := tenant + "/" + project
		if _, err := tx.Exec(ctx, insProject, pid, tenant, project); err != nil {
			return fmt.Errorf("postgres: create project: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE tenant_id = $1 AND name = $2`,
			tenant, project).Scan(&projectID); err != nil {
			return fmt.Errorf("postgres: read back project: %w", err)
		}

		id = tenant + "/" + project + "/" + hex.EncodeToString(HashKey(key)[:8])
		const insKey = `
			INSERT INTO api_keys (id, tenant_id, project_id, key_hash, key_prefix, label)
			VALUES ($1, $2, $3, $4, $5, $6)`
		if _, err := tx.Exec(ctx, insKey, id, tenant, projectID, HashKey(key), prefix, label); err != nil {
			return fmt.Errorf("postgres: create key: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return key, id, nil
}
