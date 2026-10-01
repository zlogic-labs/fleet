package authn

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Memory is an in-process KeyStore.
//
// It is the right backend for a single gateway, which is what v1 is: keys are
// metadata, a Fleet installation has as many API keys as it has teams, and the
// thing that actually has to be shared between processes — rate-limit counters
// and reservations — moves to Redis before the keys do. A gateway behind a load
// balancer with per-process counters would enforce N times the limit, so that
// ordering is not incidental and is stated here so nobody reverses it by
// accident.
type Memory struct {
	mu   sync.RWMutex
	keys map[string]MemoryKey
}

// MemoryKey is one registered credential.
type MemoryKey struct {
	Principal Principal
	// ExpiresAt is when the key stops working. Zero means it does not.
	ExpiresAt time.Time
}

// Valid reports whether the key is usable at now.
//
// The clock is a parameter rather than time.Now() so an expired key is a
// testable fact rather than something only discoverable by waiting.
func (k MemoryKey) Valid(now time.Time) bool {
	return k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt)
}

func NewMemory() *Memory {
	return &Memory{keys: map[string]MemoryKey{}}
}

var _ KeyStore = (*Memory)(nil)

// Put registers or replaces a key.
func (m *Memory) Put(key string, p Principal, expiresAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = MemoryKey{Principal: p, ExpiresAt: expiresAt}
}

// Lookup resolves a key, reporting expiry as absence.
func (m *Memory) Lookup(_ context.Context, key string) (Principal, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[key]
	if !ok || !k.Valid(time.Now()) {
		return Principal{}, false
	}
	return k.Principal, true
}

// Revoke removes one key, which is how a leaked credential is rotated without
// disturbing the tenant's balance or its other keys.
func (m *Memory) Revoke(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
}

// Keys lists the registered principals, for deriving a tenant's policy.
//
// Expired keys are included: the list answers "what did an operator declare",
// not "what may be used right now", and an expired key with a loose limit must
// not silently tighten a live key's.
func (m *Memory) Keys() []Principal {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Principal, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k.Principal)
	}
	return out
}

var _ Lister = (*Memory)(nil)

// ParsePrincipal reads "tenant/keyid:limits" — the form the seed script and a
// first deployment use.
//
// The format is deliberately small. A real installation issues keys through a
// management API that does not exist yet; until it does, this lets a gateway be
// given working credentials from an environment variable without writing a
// config file with secrets in it. Limits are "rpm/tpm" or either alone.
func ParsePrincipal(spec string) (Principal, error) {
	ident, limits, _ := strings.Cut(spec, "|")
	tenant, keyID, ok := strings.Cut(strings.TrimSpace(ident), "/")
	if !ok || tenant == "" || keyID == "" {
		return Principal{}, errs.InvalidArgument("key spec %q must be tenant/keyid", spec)
	}
	p := Principal{Tenant: tenant, KeyID: keyID, Labels: map[string]string{}}
	if limits == "" {
		return p, nil
	}
	for _, part := range strings.Split(limits, ",") {
		field, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Principal{}, errs.InvalidArgument(
				"limit %q must be name=value", part)
		}
		n, err := atoi(value)
		if err != nil {
			return Principal{}, errs.InvalidArgument("limit %q must be a number", value)
		}
		switch field {
		case "rpm":
			p.RateLimit.RequestsPerMinute = n
		case "tpm":
			p.RateLimit.TokensPerMinute = n
		default:
			return Principal{}, errs.InvalidArgument(
				"unknown limit %q; use rpm or tpm", field)
		}
	}
	return p, nil
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errs.InvalidArgument("empty number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errs.InvalidArgument("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
