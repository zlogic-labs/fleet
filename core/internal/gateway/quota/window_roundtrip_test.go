package quota

import (
	"testing"
	"time"
)

// The console builds a rule's delete id from a window it recovered out of
// seconds, so every spelling it can produce has to parse back to the same
// duration. Rounding is the risk: 90 seconds rendered as "2m" would delete a
// different rule than the one on screen, and would leave the displayed rule
// behind.
func TestEveryWindowSpellingRoundTrips(t *testing.T) {
	cases := map[string]time.Duration{
		"1mo":  30 * 24 * time.Hour,
		"2mo":  60 * 24 * time.Hour,
		"1w":   7 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"1d":   24 * time.Hour,
		"5h":   5 * time.Hour,
		"90m":  90 * time.Minute,
		"45m":  45 * time.Minute,
		"30m":  30 * time.Minute,
		"90s":  90 * time.Second,
		"1s":   time.Second,
		"30d":  30 * 24 * time.Hour,
		"360h": 360 * time.Hour,
	}
	for text, want := range cases {
		got, err := ParseDuration(text)
		if err != nil {
			t.Errorf("ParseDuration(%q) errored: %v", text, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", text, got, want)
		}
	}
}
