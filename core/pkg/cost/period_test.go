package cost

import (
	"testing"
	"time"
)

func periodOf(year int, month time.Month) Period {
	return NewPeriod(time.Date(year, month, 15, 12, 0, 0, 0, time.UTC))
}

func TestPreviousStepsBackOneCalendarMonth(t *testing.T) {
	got := periodOf(2026, time.October).Previous()
	if got.String() != "2026-09" {
		t.Fatalf("October's previous month is %s", got)
	}
	got = periodOf(2026, time.January).Previous()
	if got.String() != "2025-12" {
		t.Fatalf("January's previous month is %s, and it is not in the same year", got)
	}
}

// The month-length trap. Go normalises 31 March minus one month into 3 March, so
// a previous month computed from a day that does not exist in the earlier month
// lands on the month that is already running -- which is the month a comparison
// is least able to answer for, and it would look like a working report.
func TestPreviousSurvivesAMonthThatIsLongerThanItsPredecessor(t *testing.T) {
	mar31 := NewPeriod(time.Date(2026, time.March, 31, 23, 0, 0, 0, time.UTC))
	if got := mar31.Previous(); got.String() != "2026-02" {
		t.Fatalf("31 March's previous month is %s, want 2026-02", got)
	}
	may31 := NewPeriod(time.Date(2026, time.May, 31, 23, 0, 0, 0, time.UTC))
	if got := may31.Previous(); got.String() != "2026-04" {
		t.Fatalf("31 May's previous month is %s, want 2026-04", got)
	}
}

func TestPreviousIsHalfOpenAndContiguous(t *testing.T) {
	oct := periodOf(2026, time.October)
	sep := oct.Previous()
	if !sep.End.Equal(oct.Start) {
		t.Fatalf("the two months leave a gap or overlap: %s ends at %s, %s starts at %s",
			sep, sep.End, oct, oct.Start)
	}
	if sep.Contains(oct.Start) {
		t.Fatal("a month contains the instant its successor begins")
	}
}
