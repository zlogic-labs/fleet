package main

import (
	"strings"
	"testing"
)

// The comma split is what the install script relies on: it writes one env
// value holding every operator's token, and fleet-apiserver is given no
// --admin-token flag at all. If this stops splitting, the whole list arrives as
// one token, none of them match, and every management request is refused while
// the operator reads it as a broken install rather than a parse mistake.

func TestACommaSeparatedEnvironmentValueBecomesSeveralTokens(t *testing.T) {
	t.Setenv("FLEET_TEST_TOKENS", "first,second,third")

	var got tokens
	got.seed("FLEET_TEST_TOKENS")

	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A repeat of the flag and a list in the environment have to add up rather than
// replace each other, or naming the flag on a unit file silently drops whatever
// the environment already supplied.
func TestTheFlagAddsToWhatTheEnvironmentSupplied(t *testing.T) {
	t.Setenv("FLEET_TEST_TOKENS", "from-env")

	var got tokens
	got.seed("FLEET_TEST_TOKENS")
	if err := got.Set("from-flag"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got.String() != "from-env,from-flag" {
		t.Fatalf("got %q", got.String())
	}
}

func TestSpacesAroundTokensAreTrimmedAndBlanksDropped(t *testing.T) {
	t.Setenv("FLEET_TEST_TOKENS", " a , ,b,   ")

	var got tokens
	got.seed("FLEET_TEST_TOKENS")

	if got.String() != "a,b" {
		t.Fatalf("got %q, want \"a,b\"", got.String())
	}
}

// An empty flag value would otherwise look configured while matching nothing.
func TestAnEmptyFlagValueIsRejected(t *testing.T) {
	var got tokens
	if err := got.Set("   "); err == nil {
		t.Fatal("an empty token was accepted")
	}
	if len(got) != 0 {
		t.Fatalf("a rejected value still appended %v", got)
	}
}

func TestAnAbsentEnvironmentVariableSeedsNothing(t *testing.T) {
	var got tokens
	got.seed("FLEET_TEST_TOKENS_DEFINITELY_NOT_SET")
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestStringRoundTripsThroughTheFlagSyntax(t *testing.T) {
	// String is what the flag package prints in usage output, and its result is
	// what a careless copy-paste would feed back in. Both directions must work.
	var original tokens
	for _, v := range []string{"one", "two"} {
		if err := original.Set(v); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	var reparsed tokens
	if err := reparsed.Set(original.String()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if strings.Join(reparsed, "|") != strings.Join(original, "|") {
		t.Fatalf("%v became %v", original, reparsed)
	}
}
