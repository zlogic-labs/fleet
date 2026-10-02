package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/gateway/quota"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The budget, against a real database.
//
// The claim under test is not "a tenant is refused when it overspends" — it is
// "a tenant is never *allowed* to overspend". A check-then-act implementation
// passes the first test and fails the rest, and fails the rest only under
// concurrency, which is exactly when a real customer is present.

func TestSpendIsReservedThenSettled(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 1000, 24*time.Hour)

	r := mustReserve(t, q, scope, usageFor(60, 0))
	if got, want := used(t, q, scope, quota.TokensTotal), int64(60); got != want {
		t.Errorf("committed = %d after reserving, want %d", got, want)
	}
	q.Settle(context.Background(), r, estimate(usageFor(7, 0)))
	if got, want := used(t, q, scope, quota.TokensTotal), int64(7); got != want {
		t.Errorf("committed = %d after settling 7, want 7 — the reservation was not released", got)
	}
}

// The reservation is an upper bound, not the bill. Settling for less must give
// the difference back, or a tenant is charged the worst case of every request
// it ever made.
func TestSettlingBelowTheBoundRefundsTheDifference(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 10_000, 24*time.Hour)

	for i := 0; i < 5; i++ {
		q.Settle(context.Background(), mustReserve(t, q, scope, usageFor(100, 0)), estimate(usageFor(3, 0)))
	}
	if got, want := used(t, q, scope, quota.TokensTotal), int64(15); got != want {
		t.Errorf("committed = %d after five requests of 3, want %d — the bounds were not refunded", got, want)
	}
}

// A rule that cannot cover the bound refuses it whole. A partial commit would
// be worse than useless: the request is refused anyway, having moved a counter
// first.
func TestABoundOverTheBudgetIsRefusedWhole(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 100, 24*time.Hour)

	if _, err := tryReserve(q, scope, usageFor(101, 0)); quota.AsExceeded(err) == nil {
		t.Fatalf("a bound of 101 against a budget of 100 was admitted: %v", err)
	}
	if got := used(t, q, scope, quota.TokensTotal); got != 0 {
		t.Errorf("committed = %d after a refusal, want 0 — a refused reservation left a mark", got)
	}
}

// Tokens are not one number. A cached prompt is a fraction of the prompt, and
// a rule on fresh prompt tokens must see the difference — otherwise a budget
// meant to cap what was actually paid for would be enforced against traffic the
// engine served from its cache.
func TestEachDimensionIsCountedSeparately(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	// A prompt of 1000 of which 900 came from the cache.
	p := usageFor(1000, 0)
	p.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: 900}

	rule(t, q, quota.KindTenant, "acme", quota.TokensFresh, 5000, time.Hour)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 5000, time.Hour)
	mustReserve(t, q, scope, p)

	if got, want := used(t, q, scope, quota.TokensFresh), int64(100); got != want {
		t.Errorf("fresh tokens = %d, want %d — the cached 900 were counted as fresh", got, want)
	}
	// The same request reads as its full prompt under a total rule, which is the
	// point of having both: the two rules are answering different questions
	// about one request.
	if got, want := used(t, q, scope, quota.TokensTotal), int64(1000); got != want {
		t.Errorf("total tokens = %d, want %d", got, want)
	}
}

// Money and tokens are separate caps, not alternatives. A tenant can be
// comfortably inside one and far outside the other, and the cheaper-to-express
// rule is not a substitute for the other.
func TestTokenAndMoneyRulesAreEnforcedTogether(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 1_000_000, time.Hour)
	// A generous token allowance, a tight money one.
	rule(t, q, quota.KindTenant, "acme", quota.Units, 10, time.Hour)

	// Well inside the token rule, over the money one.
	u := usageFor(100, 0)
	big := estimate(u)
	big.Amount = 25

	if _, err := q.Reserve(context.Background(),
		quota.Request{Scope: scope, Estimate: big, Now: time.Now()}); quota.AsExceeded(err) == nil {
		t.Fatal("a request inside its token budget but over its money budget was admitted")
	}
	// Nothing was written, even for the token rule that would have passed.
	if got := used(t, q, scope, quota.TokensTotal); got != 0 {
		t.Errorf("tokens committed = %d after a refusal by the money rule", got)
	}
}

// A project's rules and its tenant's are both enforced, and neither is a
// substitute for the other.
func TestProjectRulesAndTenantRulesBothApply(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 1_000_000, time.Hour)
	rule(t, q, quota.KindProject, "acme/research", quota.TokensTotal, 100, time.Hour)

	// Inside the tenant's allowance, outside the project's.
	if _, err := tryReserve(q, scope, usageFor(101, 0)); quota.AsExceeded(err) == nil {
		t.Fatal("a project spent past its own budget")
	}
	// And the project rule is the one that reported it, so the tenant is told
	// which of their scopes is the problem.
	_, err := tryReserve(q, scope, usageFor(101, 0))
	if e := quota.AsExceeded(err); e == nil || e.Kind != quota.KindProject {
		t.Errorf("refusal kind = %v, want the project", e)
	}
}

// A rolling window forgets. Spending two hours ago must stop counting once it
// is two hours out of a two-hour rule, which is the property an anchored
// counter cannot have and is the reason the counters are bucketed at all.
func TestTheWindowActuallySlides(t *testing.T) {
	db := testDB(t)
	truncateSpend(t, db)
	q, scope := budgetFixture(t, db)
	rule(t, q, quota.KindTenant, "acme", quota.TokensTotal, 100, 2*time.Hour)

	// Spend 60 two hours and one bucket ago, where a 2h rule cannot see it.
	q.db.pool.Exec(context.Background(),
		`INSERT INTO spend_counters (scope, bucket_start, tokens_total_spent)
		 VALUES ($1, $2, 60)`, scope.Tenant,
		quota.BucketStart(time.Now().Add(-3*time.Hour), quota.ResolutionFor(2*time.Hour)))

	mustReserve(t, q, scope, usageFor(90, 0))
	if got := used(t, q, scope, quota.TokensTotal); got != 90 {
		t.Errorf("committed = %d, want 90 — spending from outside the window still counted", got)
	}
}

// Concurrency is the only reason reservation exists. Many requests racing for
// the last of a budget must not collectively pass it.

func budgetFixture(t *testing.T, db *DB) (*Quota, ratelimit.Scope) {
	t.Helper()
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ('acme','Acme')
		ON CONFLICT (id) DO NOTHING`)
	mustExec(t, db, `INSERT INTO projects (id, tenant_id, name) VALUES ('acme/research','acme','research')
		ON CONFLICT (id) DO NOTHING`)
	return NewQuota(db), ratelimit.Scope{Tenant: "acme", Project: "research"}
}

func rule(t *testing.T, q *Quota, kind quota.ScopeKind, id string,
	d quota.Dimension, limit int64, window time.Duration) {
	t.Helper()
	if err := q.PutRule(context.Background(), quota.Rule{
		ScopeKind: kind, ScopeID: id, Dimension: d, Limit: limit, Window: window,
	}); err != nil {
		t.Fatalf("put rule %s %s: %v", kind, d, err)
	}
}

func truncateSpend(t *testing.T, db *DB) {
	t.Helper()
	truncate(t, db, "spend_counters", "budget_rules", "api_keys", "projects", "tenants")
}

func usageFor(prompt, completion int) openai.Usage {
	return openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

func estimate(u openai.Usage) quota.Estimate {
	return quota.Estimate{Usage: u, Known: true}
}

func tryReserve(q *Quota, scope ratelimit.Scope, u openai.Usage) (quota.Reservation, error) {
	return q.Reserve(context.Background(), quota.Request{
		Scope: scope, Estimate: estimate(u), Now: time.Now(),
	})
}

func mustReserve(t *testing.T, q *Quota, scope ratelimit.Scope, u openai.Usage) quota.Reservation {
	t.Helper()
	r, err := tryReserve(q, scope, u)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}

func used(t *testing.T, q *Quota, scope ratelimit.Scope, d quota.Dimension) int64 {
	t.Helper()
	got, err := q.Used(context.Background(), scope)
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	return got[d]
}
