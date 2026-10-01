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

// ParsePrincipal reads "tenant/project/keyid" — the form the seed script and a
// first deployment use.
//
// The format is deliberately small. A real installation issues keys through a
// management API that does not exist yet; until it does, this lets a gateway be
// given working credentials from an environment variable without writing a
// config file with secrets in it.
//
// The project is required rather than optional. A key without one would have
// no partition to be limited in, and the resulting asymmetry — some keys
// charged to a project and some only to the tenant — would make "how much can
// this project spend" depend on which key a caller happened to hold.
func ParsePrincipal(spec string) (Principal, error) {
	if strings.Contains(spec, "|") {
		// Rejected rather than ignored. A key spec carrying limits is an old
		// configuration, and quietly dropping the numbers would leave an
		// operator believing a budget was in force. Saying where they moved is
		// worth a line of error text.
		return Principal{}, errs.InvalidArgument(
			"key spec %q carries limits; rate limits belong to a tenant or a project now, "+
				"set them under rate_limits.tenants or rate_limits.projects", spec)
	}
	parts, err := Split(spec, 3)
	if err != nil {
		return Principal{}, errs.InvalidArgument(
			"key spec %q must be tenant/project/keyid: %s", spec, err)
	}
	return Principal{
		Tenant:  parts[0],
		Project: parts[1],
		KeyID:   parts[2],
		Labels:  map[string]string{},
	}, nil
}

// Split divides a slash-separated name into exactly n parts.
//
// Exactly n, not up to n. The three callers are a key spec (3), a project
// limit (2) and a tenant limit (1), and every one of them has a spelling where
// a missing part is plausible: "acme/admin" as a key without a project, or
// "acme/research" as a tenant limit on a name that happens to contain a slash.
// Accepting the short form would put those in buckets nothing reads, which is
// a limit that never applies — the failure mode of a misconfiguration that
// looks like it worked.
func Split(spec string, n int) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(spec), "/")
	if len(parts) != n {
		return nil, errs.InvalidArgument("expected %d slash-separated parts, got %d", n, len(parts))
	}
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
		if parts[i] == "" {
			return nil, errs.InvalidArgument("part %d is empty", i+1)
		}
	}
	return parts, nil
}

// ParseLimits reads the "rpm=…,tpm=…" suffix of a limit declaration.
//
// Either name alone is fine; an unknown name is an error rather than a skipped
// field, because "rps" instead of "rpm" would otherwise leave a deployment
// with no request limit and no complaint.
func ParseLimits(spec string) (Limits, error) {
	var l Limits
	for _, part := range strings.Split(spec, ",") {
		field, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Limits{}, errs.InvalidArgument("limit %q must be name=value", part)
		}
		n, err := atoi(value)
		if err != nil {
			return Limits{}, errs.InvalidArgument("limit %q must be a number", value)
		}
		switch strings.TrimSpace(field) {
		case "rpm":
			l.RequestsPerMinute = n
		case "tpm":
			l.TokensPerMinute = n
		default:
			return Limits{}, errs.InvalidArgument("unknown limit %q; use rpm or tpm", field)
		}
	}
	return l, nil
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
