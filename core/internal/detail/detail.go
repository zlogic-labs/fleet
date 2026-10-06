// Package detail mirrors the ledger into a store shaped for reporting.
//
// It exists because three queries want a different shape than Postgres, and one
// of them cannot afford to be slow:
//
//   - closing a period scans a whole month of usage and must return the same
//     answer every time, which needs a consistent snapshot and a row lock;
//   - the cost pool's as-of query needs the GPU shape of a deployment at the
//     moment of each request, which is a lateral join against a time series;
//   - the metering audit groups by endpoint and window, which is a scan.
//
// All three are analytics over the same rows, and all three read a copy.
//
// The copy is not the ledger. Postgres is. That is the whole safety argument:
// every figure this package serves can be recomputed from the authoritative
// table, so losing the mirror costs a rebuild and nothing else. It follows that
// nothing here may affect whether a request succeeds -- see Sink.Enqueue, which
// cannot fail.
package detail

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Sink accepts settled records for the detail store.
//
// Enqueue must not block and must not fail. The caller is on the path that has
// already forwarded a response and written the authoritative row; anything this
// method returns would be a second way for a reporting replica to take down a
// serving path. Implementations drop what they cannot hold and say so through
// OnDrop.
type Sink interface {
	Enqueue(r billing.Record)
	Close() error
}

// Nop accepts everything and does nothing, which is the correct behaviour when
// no detail store is configured: not a degraded mode, just the shape with no
// store behind it.
type Nop struct{}

func (Nop) Enqueue(billing.Record) {}
func (Nop) Close() error           { return nil }

// OnDrop reports records the sink could not accept. It is a callback rather
// than an error because there is nobody left to return it to.
type OnDrop func(int)

// Queue is a buffered Sink that hands batches to a writer.
//
// The buffer is a ring of bounded size and the policy on a full buffer is to
// drop the arriving record and keep the ones already queued. Dropping the
// arrival rather than the oldest is deliberate: the queued records are older,
// so blocking to make room for a newer one trades a bounded gap for latency on
// every request. A gap is recoverable -- the mirror can be rebuilt from Postgres
// -- and unbounded latency is not.
type Queue struct {
	ch     chan billing.Record
	writer Writer
	log    *slog.Logger
	onDrop OnDrop

	closeOnce sync.Once
	done      chan struct{}
}

// Writer accepts batches. It runs on the queue's goroutine, one at a time, so it
// need not be safe for concurrent use.
type Writer interface {
	Write(ctx context.Context, batch []billing.Record) error
	Close() error
}

const (
	// defaultCapacity is sized in requests rather than bytes because the
	// records differ in size and a count bound is the only one that cannot be
	// defeated by an unusually long prompt.
	defaultCapacity = 8192
	// defaultFlush is the longest a record waits before being written. A detail
	// store that is ten minutes behind is still correct; one that never flushes
	// is a silent sink.
	defaultFlush = 5 * time.Second
)

// NewQueue starts a queue of the default size. A writer error is logged and the
// batch is dropped: there is no retry queue, because a retry that outruns the
// outage would grow without bound and the records are reconstructible.
func NewQueue(w Writer, log *slog.Logger, onDrop OnDrop) *Queue {
	return newQueue(w, log, onDrop, defaultCapacity, defaultFlush)
}

func newQueue(w Writer, log *slog.Logger, onDrop OnDrop, capacity int, flush time.Duration) *Queue {
	q := &Queue{
		ch:     make(chan billing.Record, capacity),
		writer: w,
		log:    log,
		onDrop: onDrop,
		done:   make(chan struct{}),
	}
	go q.run(flush)
	return q
}

func (q *Queue) Enqueue(r billing.Record) {
	select {
	case q.ch <- r:
	default:
		if q.onDrop != nil {
			q.onDrop(1)
		}
	}
}

// run drains the queue until Close.
//
// Shutdown is a receive from a closed channel and nothing else. An earlier
// version also selected on q.done, which this function closes when it returns —
// so that case could never fire, while a closed q.ch reports ready forever. The
// result was that Close never returned and this loop received the zero Record
// from the closed channel in a tight loop, handing 8192-record batches of blank
// rows to the writer for as long as the process survived. A gateway restart
// wrote thirteen million empty rows into the replica, which is how the
// reconciliation report came to be the first thing in the product to notice
// (§11.14).
func (q *Queue) run(flush time.Duration) {
	defer close(q.done)
	ticker := time.NewTicker(flush)
	defer ticker.Stop()

	batch := make([]billing.Record, 0, defaultCapacity)
	for {
		select {
		case r, ok := <-q.ch:
			if !ok {
				// A closed channel delivers what is buffered before it reports
				// the close, so everything queued has already been received and
				// one last flush is the whole of the drain.
				q.flush(batch)
				return
			}
			batch = append(batch, r)
			// Drain whatever else is already queued before writing, so a burst
			// becomes one batch instead of one batch per record.
			for len(batch) < cap(batch) {
				next, ok := <-q.ch
				if !ok {
					q.flush(batch)
					return
				}
				batch = append(batch, next)
			}
			q.flush(batch)
			batch = batch[:0]
		case <-ticker.C:
			q.flush(batch)
			batch = batch[:0]
		}
	}
}

func (q *Queue) flush(batch []billing.Record) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := q.writer.Write(ctx, batch); err != nil {
		q.log.Warn("the detail store did not take a batch; it can be rebuilt from postgres",
			"records", len(batch), "err", err)
		if q.onDrop != nil {
			q.onDrop(len(batch))
		}
	}
}

// Close stops the queue and closes the writer.
//
// It drains what is queued rather than dropping it: closing the channel is the
// signal, and a closed channel delivers its contents before it reports the
// close, so the run loop receives every buffered record and flushes once on the
// way out. The common case for a restart is "the queue had a little in it" and
// that little is exactly what a rebuild would have to go back for.
func (q *Queue) Close() error {
	var err error
	q.closeOnce.Do(func() {
		close(q.ch)
		<-q.done
		err = q.writer.Close()
	})
	return err
}
