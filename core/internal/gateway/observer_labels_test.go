package gateway

import (
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/prom"
)

// Every series a served request produces, labelled with its tenant and project.
//
// The other tests in this package look a series up by the labels they care
// about, and a lookup with no labels matches anything -- so a gateway that
// published every request against an empty tenant passed all of them. This one
// asserts the whole label set, which is the thing an operator reads.

func seriesFor(t *testing.T, exposition, name string) []prom.Sample {
	t.Helper()
	var out []prom.Sample
	for _, s := range prom.Parse([]byte(exposition)) {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func labelsOf(s prom.Sample) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"tenant", "project", "model", "outcome", "kind"} {
		if v, ok := s.Label(k); ok {
			out[k] = v
		}
	}
	return out
}

func TestAServedRequestNamesItsTenantAndProject(t *testing.T) {
	reg, obs := newTestObserver()
	obs.Served(record(billing.Record{Endpoint: "e", Amount: 1}), time.Second, time.Millisecond, true)

	got := seriesFor(t, reg.String(), "fleet_requests_total")
	if len(got) != 1 {
		t.Fatalf("expected one request series, got %d", len(got))
	}
	if want := (map[string]string{
		"tenant": "acme", "project": "research", "model": "gpt-4o", "outcome": "ok",
	}); !equalLabels(labelsOf(got[0]), want) {
		t.Errorf("request series labels %v, want %v", labelsOf(got[0]), want)
	}

	tokens := seriesFor(t, reg.String(), "fleet_tokens_total")
	if len(tokens) == 0 {
		t.Fatal("no token series")
	}
	for _, s := range tokens {
		l := labelsOf(s)
		if l["tenant"] != "acme" || l["project"] != "research" || l["model"] != "gpt-4o" {
			t.Errorf("token series %s is labelled %v, want tenant=acme project=research model=gpt-4o",
				s.Name, l)
		}
		if l["kind"] == "" {
			t.Errorf("token series is not labelled with a billing side: %v", l)
		}
	}
}

func equalLabels(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
