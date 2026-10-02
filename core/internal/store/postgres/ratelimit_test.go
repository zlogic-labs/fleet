package postgres

import (
	"context"
	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"testing"
	"time"
)

// Settling, refunds, window edges and pruning, against a real database.

func truncateRate(t *testing.T, db *DB) {
	t.Helper()
	truncate(t, db, "rate_counters", "spend_counters", "budget_rules",
		"api_keys", "projects", "tenants")
}

// rateFixture returns two limiters over one database, standing in for two
// gateway replicas. They share nothing but the tables.
func rateFixture(t *testing.T, db *DB, p ratelimit.Policy) (*RateLimiter, *RateLimiter) {
	t.Helper()
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme')
		ON CONFLICT (id) DO NOTHING`)
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name) VALUES ('acme/research','acme','research')
		ON CONFLICT (id) DO NOTHING`)
	policyFor := func(ratelimit.Scope) ratelimit.Policies {
		return ratelimit.Policies{Envelope: p, Partition: p}
	}
	return NewRateLimiter(db, policyFor), NewRateLimiter(db, policyFor)
}

func mustReserveRate(t *testing.T, l *RateLimiter, scope ratelimit.Scope, tokens int) ratelimit.Reservation {
	t.Helper()
	r, err := l.Reserve(context.Background(), ratelimit.Request{Scope: scope, Tokens: tokens})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

// snapshotRequests reads what the limiter itself can still see, which is the
// only reading that has the window applied to it.
func snapshotRequests(t *testing.T, l *RateLimiter, scope string) int {
	t.Helper()
	for _, u := range l.Snapshot(context.Background()) {
		if u.Scope.Tenant == scope && u.Scope.Project == "" {
			return u.RequestsThisMinute
		}
	}
	return 0
}

func rateUsed(t *testing.T, db *DB, scope, column string) int64 {
	t.Helper()
	var stmt string
	switch column {
	case "requests":
		stmt = `SELECT COALESCE(SUM(requests), 0) FROM rate_counters WHERE scope = $1`
	case "tokens":
		stmt = `SELECT COALESCE(SUM(reserved), 0) + COALESCE(SUM(settled), 0) FROM rate_counters WHERE scope = $1`
	default:
		t.Fatalf("unknown column %q", column)
	}
	var got int64
	if err := db.pool.QueryRow(context.Background(), stmt, scope).Scan(&got); err != nil {
		t.Fatalf("read %s for %s: %v", column, scope, err)
	}
	return got
}

func TestTheWindowSlides(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: 10})

	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }

	for i := 0; i < 10; i++ {
		mustReserveRate(t, l, ratelimit.Tenant("acme"), 1)
	}
	if _, err := l.Reserve(context.Background(), ratelimit.Request{Scope: ratelimit.Tenant("acme"), Tokens: 1}); err == nil {
		t.Fatal("the eleventh request passed a limit of 10")
	}

	l.now = func() time.Time { return base.Add(61 * time.Second) }
	mustReserveRate(t, l, ratelimit.Tenant("acme"), 1)

	if got := snapshotRequests(t, l, "acme"); got != 1 {
		t.Errorf("after the window rolled, %d requests still counted, want 1", got)
	}

	// The rows are still in the table, which is why something has to delete
	// them. Nothing on the request path does.
	//
	// One row, not ten: the ten reservations were all made at the same pinned
	// second, and a bucket is a second. A real gateway spreads them over
	// whatever seconds it takes.
	cut, err := l.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if cut != 1 {
		t.Errorf("pruned %d rows, want the 1 that aged out", cut)
	}
	if got := rateUsed(t, db, "acme", "requests"); got != 1 {
		t.Errorf("after pruning, %d rows remain, want only the live one", got)
	}
}

// With no budget rules declared there is still spend to clear, and the longest
// window lookup returns a NULL rather than no rows.
func TestPruningWithNoRulesAtAll(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	q, _ := budgetFixture(t, db)
	old := quota.BucketStart(time.Now().Add(-48*time.Hour), time.Hour)
	mustExec(t, db, `INSERT INTO spend_counters (scope, bucket_start, tokens_total_spent)
		VALUES ('acme', $1, 1)`, old)

	if _, err := q.Prune(context.Background()); err != nil {
		t.Fatalf("prune with no rules: %v", err)
	}
	var left int
	if err := db.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM spend_counters WHERE scope = 'acme'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rows survived a prune with no rules to read them", left)
	}
}

func TestSnapshotLabelsBothLevels(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	l, _ := rateFixture(t, db, ratelimit.Policy{RequestsPerMinute: 100})
	mustReserveRate(t, l, ratelimit.Project("acme", "research"), 25)

	var sawTenant, sawProject bool
	for _, u := range l.Snapshot(context.Background()) {
		if u.Scope.Tenant != "acme" {
			t.Errorf("unexpected scope %+v", u.Scope)
		}
		if u.TokensReserved != 25 || u.InFlight != 1 {
			t.Errorf("unexpected standing %+v", u)
		}
		switch u.Scope.Project {
		case "":
			sawTenant = u.Kind == ratelimit.KindTenant
		case "research":
			sawProject = u.Kind == ratelimit.KindProject
		}
	}
	if !sawTenant || !sawProject {
		t.Errorf("snapshot labelled tenant=%v project=%v, want both", sawTenant, sawProject)
	}
}

// No limits at all is the common case — an evaluation, a laptop, every tenant
// nobody has configured. It must not write a row.
func TestUnlimitedScopesTouchNoCounter(t *testing.T) {
	db := testDB(t)
	truncateRate(t, db)
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme') ON CONFLICT DO NOTHING`)
	l := NewRateLimiter(db, func(ratelimit.Scope) ratelimit.Policies { return ratelimit.Policies{} })

	r, err := l.Reserve(context.Background(),
		ratelimit.Request{Scope: ratelimit.Tenant("acme"), Tokens: 1000})
	if err != nil {
		t.Fatalf("an unlimited scope was refused: %v", err)
	}
	if r.WasCounted() {
		t.Error("an unlimited scope took a request slot")
	}
	l.Settle(context.Background(), r, 900)
	if got := rateUsed(t, db, "acme", "tokens"); got != 0 {
		t.Errorf("an unlimited scope wrote %d tokens", got)
	}
}
