package handler

import (
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The rolling sample log behind the console's "Recent requests" table.
//
// Separate from the handler because it is a different lifetime: the log
// outlives every request and is read by the status endpoint, and it is the
// only state the chat handler keeps.

// Sample is one completed request, as the console shows it.
//
// Tenant and Project are carried because the console answers "who spent this"
// and a rolling log that cannot say is only a performance panel. The ledger
// has it; this is the one that survives without a database.
type Sample struct {
	Tenant     string
	Project    string
	Model      string
	Endpoint   string
	TTFT       time.Duration
	Duration   time.Duration
	Bytes      int64
	Usage      *openai.Usage
	UsageKnown bool
	PromptEst  int
	Requested  int
	Streamed   bool
}

// SampleLog keeps the last limit requests.
//
// Bounded and in memory on purpose: this is a dashboard, not an audit trail.
// The durable record is the ledger, and a gateway that buffered every request
// for a month would be a gateway whose memory is sized by its traffic.
type SampleLog struct {
	mu      sync.RWMutex
	samples []Sample
	limit   int
}

func NewSampleLog(limit int) *SampleLog {
	return &SampleLog{limit: limit}
}

func (l *SampleLog) Add(s Sample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples = append(l.samples, s)
	if len(l.samples) > l.limit {
		l.samples = l.samples[len(l.samples)-l.limit:]
	}
}

// Snapshot copies, so the status handler can serialise without holding the
// lock and without racing the next Add.
func (l *SampleLog) Snapshot() []Sample {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Sample(nil), l.samples...)
}
