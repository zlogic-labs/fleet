package apiserver

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// A period the caller typed wrong is the caller's mistake, not a fault.
//
// The console puts a query parameter straight into this, and a parse error with
// no kind reaches the client as "internal error" — which sends an operator to
// the logs over a typo and tells a script that the request might work later.
func TestAPeriodThatIsNotAMonthIsTheCallersMistake(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/api/v1/usage-reconciliation?period=september", nil)

	_, err := s.comparisonPeriod(r.Context(), r, time.Now().UTC())
	if err == nil {
		t.Fatal("a period that is not YYYY-MM was accepted")
	}
	if got := errs.KindOf(err); got != errs.KindInvalidArgument {
		t.Fatalf("want an invalid-argument kind, got %v: %v", got, err)
	}
}

// And a period that is one is parsed rather than looked up, so naming a month
// does not require the control plane to have closed anything yet.
func TestAPeriodThatIsAMonthIsTakenAsGiven(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/api/v1/usage-reconciliation?period=2026-03", nil)

	period, err := s.comparisonPeriod(r.Context(), r, time.Now().UTC())
	if err != nil {
		t.Fatalf("2026-03 was rejected: %v", err)
	}
	if period.String() != "2026-03" {
		t.Fatalf("want 2026-03, got %s", period)
	}
}
