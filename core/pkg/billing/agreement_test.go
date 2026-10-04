package billing

import "testing"

func obs(key string, out int64, fromEngine bool, src Source) Observation {
	return Observation{Key: key, Output: out, FromEngine: fromEngine, Source: src}
}

func engine(key string, out int64) Observation { return obs(key, out, true, SourceEngine) }
func counted(key string, out int64) Observation {
	return obs(key, out, false, SourceCounted)
}
func reserved(key string, out int64) Observation {
	return obs(key, out, false, SourceReserved)
}

// The population is built the same way every time so a test reads as "these two
// measurements agree" rather than as a page of setup.
func pair(key string, engineTotal, countedTotal int64, n int) []Observation {
	out := make([]Observation, 0, 2*n)
	for i := int64(0); i < int64(n); i++ {
		out = append(out, engine(key, engineTotal/int64(n)), counted(key, countedTotal/int64(n)))
	}
	return out
}

func TestTwoMeasurementsThatAgreeAreNotAFault(t *testing.T) {
	got := Verify(pair("qwen-7b", 1000, 1010, 10), DefaultMinSamples, DefaultTolerance)
	if len(got) != 1 {
		t.Fatalf("got %d keys, want 1", len(got))
	}
	if got[0].Fault {
		t.Errorf("a 1%% divergence was called a fault: %s", got[0].Reason)
	}
	if got[0].Percent == 0 {
		t.Error("the divergence was reported as exactly zero; it was truncated, not rounded")
	}
}

func TestAGatewayCountingShortIsAFault(t *testing.T) {
	got := Verify(pair("qwen-7b", 2000, 1000, 10), DefaultMinSamples, DefaultTolerance)
	if !got[0].Fault {
		t.Fatal("a gateway counting half the engine's tokens was not reported")
	}
	if got[0].Percent != -50 {
		t.Errorf("percent = %d, want -50", got[0].Percent)
	}
}

// The opposite direction matters as much and reads worse in a report: 40% off
// and 140% both mean "something is wrong", and a check that clamped the ratio
// would call one of them an improvement.
func TestAGatewayCountingLongIsAlsoAFault(t *testing.T) {
	got := Verify(pair("qwen-7b", 1000, 1400, 10), DefaultMinSamples, DefaultTolerance)
	if !got[0].Fault {
		t.Fatal("a gateway counting 40%% too many was not reported")
	}
	if got[0].Percent != 40 {
		t.Errorf("percent = %d, want 40", got[0].Percent)
	}
}

// A deployment whose engine always reports usage has no counted population at
// all. Reporting that as a divergence would be reporting the absence of data as
// a defect, and would fire on every healthy vLLM fleet.
func TestOnePopulationAloneIsNotAFault(t *testing.T) {
	only := make([]Observation, 0, 10)
	for i := 0; i < 10; i++ {
		only = append(only, engine("vllm", 100))
	}
	got := Verify(only, DefaultMinSamples, DefaultTolerance)
	if got[0].Fault {
		t.Error("a key with no counted population was called a fault")
	}
	if Faults(got) != nil {
		t.Error("Faults returned something for a key with one population")
	}
}

func TestTooFewSamplesIsNotAFault(t *testing.T) {
	got := Verify(pair("qwen-7b", 100, 10, 2), DefaultMinSamples, DefaultTolerance)
	if got[0].Fault {
		t.Error("two observations were enough to call a fault")
	}
}

// A truncated count is a floor by construction: it sits below the engine's
// figure and arithmetic, not a fault. Reporting it as one would mean an
// operator chasing a bug that is the tool working as documented.
func TestATruncatedCountIsNotAFault(t *testing.T) {
	obs := pair("qwen-7b", 2000, 1000, 10)
	// The counted observation, not the engine one: truncation is a property of
	// the gateway's own capture, so flagging an engine row means nothing.
	obs[1].Truncated = true
	got := Verify(obs, DefaultMinSamples, DefaultTolerance)
	if got[0].Fault {
		t.Error("a truncated answer was called a fault")
	}
	if got[0].Reason == "" {
		t.Error("no reason given, so an operator cannot tell why it was skipped")
	}
}

// Reserved rows are a third state, not a disagreement: there was nothing to
// count, so they belong in neither population and must not dilute the ratio.
func TestReservedRowsCountAsNeitherMeasurement(t *testing.T) {
	obs := pair("qwen-7b", 2000, 2000, 10)
	for i := 0; i < 50; i++ {
		obs = append(obs, reserved("qwen-7b", 4096))
	}
	got := Verify(obs, DefaultMinSamples, DefaultTolerance)
	if got[0].Fault {
		t.Errorf("reserved rows moved the comparison: %+v", got[0])
	}
	if got[0].EngineN != 10 || got[0].CountedN != 10 {
		t.Errorf("populations = %d/%d, want 10/10", got[0].EngineN, got[0].CountedN)
	}
}

// The zero case that a fleet which has only just started will hit, and which
// must not divide by zero.
func TestAnEngineThatReportedNothingToCompare(t *testing.T) {
	obs := make([]Observation, 0, 10)
	for i := 0; i < 10; i++ {
		obs = append(obs, engine("qwen-7b", 0), counted("qwen-7b", 100))
	}
	got := Verify(obs, DefaultMinSamples, DefaultTolerance)
	if got[0].Fault {
		t.Error("a fault was raised against a zero denominator")
	}
}

func TestKeysComeBackInOrder(t *testing.T) {
	var obs []Observation
	for _, k := range []string{"zeta", "alpha", "mid"} {
		obs = append(obs, pair(k, 100, 100, 10)...)
	}
	got := Verify(obs, DefaultMinSamples, DefaultTolerance)
	want := []string{"alpha", "mid", "zeta"}
	for i, k := range want {
		if got[i].Key != k {
			t.Fatalf("key %d = %q, want %q — a report that reorders itself is hard to diff", i, got[i].Key, k)
		}
	}
}

// One endpoint miscounting while the rest of the fleet is fine is the case the
// check exists for, and a whole-fleet average would hide it.
func TestOneBadKeyAmongGoodOnesIsFound(t *testing.T) {
	var obs []Observation
	obs = append(obs, pair("good-a", 1000, 1000, 10)...)
	obs = append(obs, pair("good-b", 1000, 990, 10)...)
	obs = append(obs, pair("bad", 1000, 200, 10)...)

	got := Verify(obs, DefaultMinSamples, DefaultTolerance)
	if len(Faults(got)) != 1 {
		t.Fatalf("got %d faults, want 1: %+v", len(Faults(got)), got)
	}
	if Faults(got)[0].Key != "bad" {
		t.Errorf("blamed %q", Faults(got)[0].Key)
	}
}
