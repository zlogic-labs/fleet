package quota

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// What a budget can be measured in.
//
// Tokens are not one number. A prompt and a completion are priced differently,
// the cached part of a prompt is priced differently again, and on a deployment
// where prefix caching works the cached part is most of the traffic. A rule
// that said only "tokens" would therefore be either meaningless or — worse —
// quietly wrong, measuring a cheap prompt-heavy workload as if it were an
// expensive one.
type Dimension string

const (
	// TokensTotal is prompt plus completion. The only dimension that is
	// honestly "tokens", and the one to use when the price book charges both
	// the same rate.
	TokensTotal Dimension = "tokens_total"
	// TokensInput is the whole prompt, cached portion included.
	TokensInput Dimension = "tokens_input"
	// TokensOutput is the completion.
	TokensOutput Dimension = "tokens_output"
	// TokensCached is the part of the prompt served from the engine's prefix
	// cache. A subset of TokensInput, not a separate stream.
	TokensCached Dimension = "tokens_cached"
	// TokensFresh is the prompt that was *not* cached — the only part of a
	// prompt charged at the full input rate.
	//
	// It is the dimension that answers "what did this deployment actually cost
	// me". On a workload with good prefix reuse, total prompt tokens can be an
	// order of magnitude above what was billed, so a budget in total tokens
	// would stop a tenant long before they spent anything.
	TokensFresh Dimension = "tokens_fresh"
	// Units is money, in quota units.
	Units Dimension = "units"
)

// Dimensions is every dimension, for validation and for the control plane.
var Dimensions = []Dimension{
	TokensTotal, TokensInput, TokensOutput, TokensCached, TokensFresh, Units,
}

func (d Dimension) Valid() bool {
	for _, k := range Dimensions {
		if k == d {
			return true
		}
	}
	return false
}

// Measure returns this dimension's value from a usage.
//
// Units is a value rather than a rate, so it is taken from the amount the
// caller already computed — asking the dimension to price anything would mean
// two prices for the same request and a rule that could check either.
func (d Dimension) Measure(u openai.Usage, amount billing.Amount) int64 {
	switch d {
	case TokensTotal:
		return int64(u.TotalTokens)
	case TokensInput:
		return int64(u.PromptTokens)
	case TokensOutput:
		return int64(u.CompletionTokens)
	case TokensCached:
		return int64(u.CachedPromptTokens())
	case TokensFresh:
		fresh := u.PromptTokens - u.CachedPromptTokens()
		if fresh < 0 {
			// An engine claiming more cached than prompt. Clamped for the same
			// reason billing clamps it: a negative count priced as a large
			// credit is a way to mint budget.
			return 0
		}
		return int64(fresh)
	case Units:
		return int64(amount)
	default:
		return 0
	}
}

// Columns names the two accumulation columns this dimension lives in.
//
// Kept beside the dimension rather than in the store so a new dimension is one
// case here and one pair of columns, not a switch in two packages. Both values
// come from this closed set, never from a request, which is what makes it safe
// for the store to interpolate them into a statement — PostgreSQL cannot bind
// an identifier, and the only reason this is not a placeholder is that nothing
// untrusted can reach it.
func (d Dimension) Columns() (spent, reserved string) {
	switch d {
	case TokensTotal:
		return "tokens_total_spent", "tokens_total_reserved"
	case TokensInput:
		return "tokens_input_spent", "tokens_input_reserved"
	case TokensOutput:
		return "tokens_output_spent", "tokens_output_reserved"
	case TokensCached:
		return "tokens_cached_spent", "tokens_cached_reserved"
	case TokensFresh:
		return "tokens_fresh_spent", "tokens_fresh_reserved"
	case Units:
		return "units_spent_micro", "units_reserved_micro"
	default:
		return "", ""
	}
}

// ── windows ───────────────────────────────────────────────────────

// maxBuckets bounds how many rows one check has to add up.
//
// The bucket size is derived from the window so this number stays fixed while
// the duration varies. Without the bound, a 30-day window at minute resolution
// would mean 43,200 rows summed on the request path, and the cost of a budget
// check would grow with the length of the window it is checking.
const maxBuckets = 120

// ResolutionFor picks a bucket size for a window.
//
// Coarser for longer windows, because a month does not need minute resolution
// to stop a tenant and a 5-hour window does not want 43,000 rows. The floor and
// ceiling keep the extremes sane: nothing finer than a minute, nothing coarser
// than a day.
//
// The consequence is stated rather than hidden: a check is exact only to within
// one bucket at each edge, so a tenant can overshoot by the traffic in one
// bucket — up to a day on a month-long rule. That is the price of an O(buckets)
// check, and it is bounded and known rather than unbounded and surprising.
func ResolutionFor(window time.Duration) time.Duration {
	res := window / maxBuckets
	switch {
	case res < time.Minute:
		return time.Minute
	case res > 24*time.Hour:
		return 24 * time.Hour
	default:
		return res
	}
}

// ParseDuration reads a window length.
//
// Accepts Go's duration syntax plus day, week, month and year, because those
// are the words an operator writing "1 month" actually types, and rejecting
// them would push them into arithmetic they have to get right themselves.
//
// Month is 30 days, not a calendar month. A window that sometimes has 28 days
// and sometimes 31 cannot be a rolling window, and pretending otherwise would
// make a tenant's allowance depend on the date they happened to start using
// Fleet. A calendar month is still needed for invoicing, and it belongs to the
// cost pool (P8), not to a budget.
func ParseDuration(s string) (time.Duration, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Compound suffixes are the common case: "1w", "30d", "1mo".
	for _, suffix := range []struct {
		name string
		mult time.Duration
	}{
		{"months", 30 * 24 * time.Hour},
		{"month", 30 * 24 * time.Hour},
		{"mos", 30 * 24 * time.Hour},
		{"mo", 30 * 24 * time.Hour},
		{"weeks", 7 * 24 * time.Hour},
		{"week", 7 * 24 * time.Hour},
		{"wks", 7 * 24 * time.Hour},
		{"wk", 7 * 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"days", 24 * time.Hour},
		{"day", 24 * time.Hour},
		{"d", 24 * time.Hour},
		{"years", 365 * 24 * time.Hour},
		{"year", 365 * 24 * time.Hour},
		{"yrs", 365 * 24 * time.Hour},
		{"yr", 365 * 24 * time.Hour},
		{"y", 365 * 24 * time.Hour},
	} {
		if !strings.HasSuffix(v, suffix.name) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, suffix.name)), 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("duration %q must be positive", s)
		}
		return time.Duration(n * float64(suffix.mult)), nil
	}
	// Bare numbers are hours, which is the only unit in a duration spec whose
	// length nobody is surprised by. Guessing minutes would be the dangerous
	// reading: an operator meaning 500 minutes would get 500 hours.
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("duration %q must be positive", s)
		}
		return time.Duration(n * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

// BucketStart floors t to a bucket boundary.
//
// Aligned to the epoch rather than to the window's start, so the same bucket
// serves every rule that happens to use the same resolution. That matters
// because a scope with a 5-hour rule and a 1-day rule shares these rows.
func BucketStart(t time.Time, resolution time.Duration) time.Time {
	if resolution <= 0 {
		resolution = time.Minute
	}
	return t.UTC().Truncate(resolution)
}
