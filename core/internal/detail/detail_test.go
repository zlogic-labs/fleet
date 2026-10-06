package detail

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// stubWriter counts what it was handed.
//
// It can also park on a suspiciously large batch, which is what makes the
// regression below fail as an assertion rather than as a package timeout: a
// queue that spins on a closed channel hands the writer 8192 blank records per
// iteration, and without the park the test would burn a core until Go gave up on
// the whole package.
type stubWriter struct {
	park chan struct{}

	mu      sync.Mutex
	records int
	batches int
	closed  bool
}

func (w *stubWriter) Write(_ context.Context, batch []billing.Record) error {
	w.mu.Lock()
	w.records += len(batch)
	w.batches++
	big := len(batch) > 64
	w.mu.Unlock()
	if big && w.park != nil {
		<-w.park
	}
	return nil
}

func (w *stubWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *stubWriter) counts() (records, batches int, closed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.records, w.batches, w.closed
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// closeWithin runs Close with a guard, so a queue that never stops fails the
// test instead of hanging it.
func closeWithin(t *testing.T, q *Queue, w *stubWriter) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- q.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		records, batches, _ := w.counts()
		t.Fatalf("Close did not return; the writer was handed %d records in %d batches",
			records, batches)
	}
}

// The regression: closing a queue that never carried anything must write
// nothing.
//
// The zero Record is a valid Record, so a loop that receives from a closed
// channel appends one per iteration and every full batch becomes a flush of
// blank rows. On the running stack that turned each gateway restart into
// thirteen million rows of empty usage in the replica, and nothing in the
// product could see it until the reconciliation report compared the two stores.
func TestClosingAnIdleQueueWritesNothing(t *testing.T) {
	w := &stubWriter{park: make(chan struct{})}
	q := newQueue(w, quietLogger(), nil, 8, time.Hour)

	closeWithin(t, q, w)

	records, batches, closed := w.counts()
	if records != 0 || batches != 0 {
		t.Fatalf("an empty queue wrote %d records in %d batches", records, batches)
	}
	if !closed {
		t.Fatal("the writer was not closed")
	}
}

// What is queued at shutdown is written, not dropped: a closed channel delivers
// its contents before it reports the close, so the run loop sees every buffered
// record and flushes once on the way out.
func TestClosingFlushesWhatIsQueued(t *testing.T) {
	w := &stubWriter{}
	q := newQueue(w, quietLogger(), nil, 8, time.Hour)

	for i := 0; i < 5; i++ {
		q.Enqueue(billing.Record{Tenant: "acme"})
	}
	closeWithin(t, q, w)

	records, batches, closed := w.counts()
	if records != 5 {
		t.Fatalf("want 5 records written, got %d", records)
	}
	if batches != 1 {
		t.Fatalf("want one batch, got %d", batches)
	}
	if !closed {
		t.Fatal("the writer was not closed")
	}
}

// A full buffer drops the arriving record and keeps the queued ones, and says so
// through OnDrop. Nothing else may take the serving path down.
func TestAFullQueueDropsTheArrival(t *testing.T) {
	w := &stubWriter{}
	dropped := 0
	// Passed to the constructor rather than assigned afterwards: the run
	// goroutine reads it, and assigning a field of a running queue is a data
	// race the race detector would rightly report.
	q := newQueue(w, quietLogger(), func(n int) { dropped += n }, 2, time.Hour)

	// The run loop is draining, so the channel is not reliably full; the
	// assertion is the weaker one that is still worth holding: every record was
	// either accepted or reported as dropped, and the accepted ones reached the
	// writer.
	for i := 0; i < 1000; i++ {
		q.Enqueue(billing.Record{Tenant: "acme"})
	}
	closeWithin(t, q, w)

	records, _, _ := w.counts()
	if records+dropped != 1000 {
		t.Fatalf("accepted %d and dropped %d of 1000", records, dropped)
	}
}
