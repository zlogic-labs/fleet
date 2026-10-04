package postgres

import (
	"context"
	"testing"
	"time"
)

// The agreement sample is the input to a check that decides whether Fleet's
// metering is wrong. Everything it returns must be honest about which of the two
// measurements it came from, because the check's whole argument is that they
// are independent and comparable.

func agreementFixture(t *testing.T, db *DB, from time.Time) {
	t.Helper()
	rows := []struct {
		endpoint  string
		out       int
		known     bool
		source    string
		truncated bool
	}{
		{"ep-a", 100, true, "engine", false},
		{"ep-a", 110, true, "engine", false},
		{"ep-a", 98, false, "counted", false},
		{"ep-a", 102, false, "counted", false},
		// Reserved rows exist and are none of the check's business: there was
		// no text to count, so they belong to neither population.
		{"ep-a", 4096, false, "reserved", false},
		{"ep-b", 500, true, "engine", false},
		{"ep-b", 50, false, "counted", false},
	}
	for i, r := range rows {
		mustExec(t, db, `INSERT INTO usage_events
			(tenant_id, model, endpoint_id, completion_tokens, usage_known,
			 usage_source, truncated, amounts_micro, occurred_at)
			VALUES ('acme', 'demo', $1, $2, $3, $4, $5, 100, $6)`,
			r.endpoint, r.out, r.known, r.source, r.truncated,
			from.Add(time.Duration(i)*time.Minute))
	}
}

func TestTheAgreementSampleCarriesBothMeasurements(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	agreementFixture(t, db, from)

	got, err := db.Agreement(context.Background(),
		Window{From: from, To: from.AddDate(0, 1, 0)})
	if err != nil {
		t.Fatalf("agreement: %v", err)
	}

	// Seven rows, one of them reserved, so six come back.
	if len(got) != 6 {
		t.Fatalf("got %d observations, want 6 — reserved rows belong to neither population: %+v", len(got), got)
	}
	var engine, counted int
	for _, o := range got {
		if o.Key != "ep-a" && o.Key != "ep-b" {
			t.Errorf("unexpected endpoint %q", o.Key)
		}
		if o.Output <= 0 {
			t.Errorf("a zero-token row was sampled: %+v", o)
		}
		if o.FromEngine {
			engine++
			continue
		}
		counted++
		if o.Source != "counted" {
			t.Errorf("a non-engine row came back as %q", o.Source)
		}
	}
	if engine != 3 || counted != 3 {
		t.Errorf("populations = %d engine / %d counted, want 3/3", engine, counted)
	}
}

// ep-b's counted total is 10% of its engine total, which is far outside any
// tolerance. If the check cannot see that from this sample it is not reading
// what the ledger holds.
func TestAWhollyWrongEndpointIsVisibleInTheSample(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	agreementFixture(t, db, from)

	got, err := db.Agreement(context.Background(),
		Window{From: from, To: from.AddDate(0, 1, 0)})
	if err != nil {
		t.Fatalf("agreement: %v", err)
	}
	var epB int64
	for _, o := range got {
		if o.Key == "ep-b" && !o.FromEngine {
			epB += o.Output
		}
	}
	if epB != 50 {
		t.Fatalf("ep-b counted output = %d, want 50; the sample is not what the fixture wrote", epB)
	}
}

func TestTheSourceMixNamesHowEachRowWasBilled(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	agreementFixture(t, db, from)

	got, err := db.SourceMix(context.Background(),
		Window{From: from, To: from.AddDate(0, 1, 0)})
	if err != nil {
		t.Fatalf("source mix: %v", err)
	}
	if got["engine"] != 3 || got["counted"] != 3 || got["reserved"] != 1 {
		t.Errorf("mix = %v, want 3 engine / 3 counted / 1 reserved", got)
	}
}

// A fleet that has served nothing must report an empty map, not a nil that
// decodes to JSON null. The difference reads as "unknown" against "nothing yet".
func TestTheSourceMixOnAnEmptyWindowIsEmptyNotAbsent(t *testing.T) {
	db := testDB(t)
	truncate(t, db, "usage_events")
	got, err := db.SourceMix(context.Background(), Window{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("source mix: %v", err)
	}
	if got == nil {
		t.Fatal("the mix is nil; it must be an empty map so it does not serialise as null")
	}
	if len(got) != 0 {
		t.Errorf("mix = %v, want empty", got)
	}
}
