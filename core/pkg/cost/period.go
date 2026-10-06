package cost

import (
	"fmt"
	"time"
)

// Period is a calendar month in UTC, half-open [Start, End).
//
// A calendar month and not a rolling window, which is the one place Fleet
// deliberately does not follow the budget path (quota.ParseDuration treats "1mo"
// as 30 days). An invoice is anchored to the month someone pays for; a budget is
// a rate of burn someone monitors. Conflating them means the cost pool drifts a
// day at a time and nobody can say what "this month" meant.
//
// UTC because the pool is computed across clusters in timezones and a
// month-length that depends on where the operator's laptop is would make the
// same usage produce two different bills.
type Period struct {
	Start time.Time
	End   time.Time
}

const periodLayout = "2006-01"

// ParsePeriod reads a YYYY-MM period.
func ParsePeriod(text string) (Period, error) {
	start, err := time.Parse(periodLayout, text)
	if err != nil {
		return Period{}, fmt.Errorf("cost: period %q is not YYYY-MM", text)
	}
	return NewPeriod(start), nil
}

// NewPeriod returns the month containing t.
func NewPeriod(t time.Time) Period {
	start := time.Date(t.UTC().Year(), t.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	return Period{Start: start, End: start.AddDate(0, 1, 0)}
}

// String renders the period as YYYY-MM.
func (p Period) String() string { return p.Start.Format(periodLayout) }

// Previous returns the calendar month before this one.
//
// Written here rather than as AddDate(0, -1, 0) at the call site, because that
// is wrong for a day of the month that does not exist in the earlier one: Go
// normalises 31 March minus one month into 3 March, so a "previous month"
// computed from today lands on the month that is already running. Both ends here
// are the first of a month, so neither can overflow.
func (p Period) Previous() Period {
	start := p.Start.AddDate(0, -1, 0)
	return Period{Start: start, End: p.Start}
}

// Valid reports whether the period can be closed.
func (p Period) Valid() bool {
	return !p.Start.IsZero() && p.End.After(p.Start)
}

// Contains reports whether t falls inside the period.
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.Start) && t.Before(p.End)
}
