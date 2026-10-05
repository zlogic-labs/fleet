package gateway

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The queue histogram is the only Fleet metric that comes from the engine rather
// than from watching bytes, so it has two ways to be wrong that the others do
// not: counting requests the engine never timed, and converting milliseconds to
// seconds by a factor of a thousand.
func TestTheQueueHistogramOnlyCountsWhatTheEngineReported(t *testing.T) {
	reg, o := newTestObserver()
	lab := map[string]string{"tenant": "acme", "project": "research", "model": "gpt-4o"}

	// An engine that publishes nothing at all, and one that publishes a decode
	// time without a queue time. Neither has a queue figure to record.
	o.served(record(billing.Record{}), 1, -1, false)
	o.served(record(billing.Record{Engine: &billing.EngineTimings{DecodeMS: float(500)}}), 1, -1, false)
	if _, ok := find(t, reg, "fleet_request_engine_queue_seconds_count", lab); ok {
		t.Error("a request with no engine queue time was counted in the queue histogram")
	}

	// A measured zero is a measurement and must be counted. A count of 1 against
	// a value of 0 is also how a dashboard tells "no queue" from "no data".
	o.served(record(billing.Record{Engine: &billing.EngineTimings{QueueMS: float(0)}}), 1, -1, false)
	if n, ok := find(t, reg, "fleet_request_engine_queue_seconds_count", lab); !ok || n != 1 {
		t.Errorf("queue samples = %v (present %v), want 1", n, ok)
	}
	if v, ok := find(t, reg, "fleet_request_engine_queue_seconds_sum", lab); !ok || v != 0 {
		t.Errorf("queue sum = %v, want 0", v)
	}
}

// The engine reports milliseconds and Prometheus wants seconds. Getting this
// wrong scales by a thousand in whichever direction the sign of the bug lands,
// and 4102 ms written as 4102 seconds puts a four-second wait outside every
// bucket above the widest one.
func TestTheQueueHistogramConvertsMillisecondsToSeconds(t *testing.T) {
	reg, o := newTestObserver()
	o.served(record(billing.Record{Engine: &billing.EngineTimings{QueueMS: float(4102.5)}}), 5, -1, false)
	lab := map[string]string{"tenant": "acme", "project": "research", "model": "gpt-4o"}
	v, ok := find(t, reg, "fleet_request_engine_queue_seconds_sum", lab)
	if !ok {
		t.Fatal("no queue sum published")
	}
	if v < 4.10 || v > 4.11 {
		t.Errorf("queue sum = %v seconds, want about 4.1025", v)
	}
}

func float(v float64) *float64 { return &v }
