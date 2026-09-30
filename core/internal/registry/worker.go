package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// SequentialPullIDs names jobs p-1, p-2, ... The counter is atomic because
// two pulls started in the same second must not collide, and a collision would
// make one job overwrite the other's progress.
type SequentialPullIDs struct{ n atomic.Uint64 }

func (s *SequentialPullIDs) Next() string {
	return "p-" + itoa(s.n.Add(1))
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func sortModels(ms []Model) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
}

func sortClusters(cs []Cluster) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
}

func sortDeployments(ds []Deployment) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Namespace != ds[j].Namespace {
			return ds[i].Namespace < ds[j].Namespace
		}
		return ds[i].Name < ds[j].Name
	})
}

// Worker runs pulls with a bounded number of goroutines.
//
// The bound is the point. A control plane that starts one goroutine per pull
// will happily saturate a link and time out every job at once; a queue with a
// small worker count degrades predictably instead.
type Worker struct {
	Store Store
	// Concurrency is how many pulls may run at once.
	Concurrency int

	queue   chan *Pull
	ids     SequentialPullIDs
	started sync.Once
	closed  atomic.Bool
	wg      sync.WaitGroup
}

func NewWorker(store Store, concurrency int) *Worker {
	if concurrency <= 0 {
		concurrency = 2
	}
	return &Worker{Store: store, Concurrency: concurrency, queue: make(chan *Pull, 64)}
}

// NextID hands out a job id. It is exported so a caller can name a job before
// the worker exists, which keeps the handler free of construction order.
func (w *Worker) NextID() string { return w.ids.Next() }

// Start launches the workers. It is safe to call more than once.
func (w *Worker) Start(run func(context.Context, *Pull) error) {
	w.started.Do(func() {
		for i := 0; i < w.Concurrency; i++ {
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				for job := range w.queue {
					job.State = PullRunning
					w.runOne(run, job)
				}
			}()
		}
	})
}

func (w *Worker) runOne(run func(context.Context, *Pull) error, job *Pull) {
	// A fresh context per job so a cancel on one pull does not stop the
	// others; AttachCancel below points the stored job at it.
	ctx, cancel := context.WithCancel(context.Background())
	job.AttachCancel(cancel)
	defer cancel()

	err := run(ctx, job)
	// errors.Is, not ==: the puller wraps the cause with the file name it was
	// working on, so a cancelled pull surfaces as
	// "config.json: context canceled" and a bare comparison would report every
	// cancellation the operator asked for as a failure.
	switch {
	case err == nil:
		job.Finish(PullDone, "")
	case errors.Is(err, context.Canceled):
		job.Finish(PullCanceled, "canceled by operator")
	default:
		job.Finish(PullFailed, err.Error())
	}

	// The stored job is the live one; the worker mutated the same pointer, so
	// this only needs to publish the terminal state.
	_, _ = w.Store.UpdatePull(context.Background(), job.ID, func(p *Pull) {
		p.State = job.State
		p.Error = job.Error
		p.FinishedAt = job.FinishedAt
		p.Progress = job.Progress
		p.BytesDone = job.BytesDone
		p.BytesTotal = job.BytesTotal
		p.FilesDone = job.FilesDone
		p.FilesTotal = job.FilesTotal
		p.CurrentFile = job.CurrentFile
	})
}

// Enqueue registers a job and hands it to a worker. It returns the stored job
// so a handler can answer with its id.
func (w *Worker) Enqueue(job *Pull) error {
	if job.done == nil {
		job.done = make(chan struct{})
	}
	if err := w.Store.CreatePull(context.Background(), job); err != nil {
		return err
	}
	w.queue <- job
	return nil
}

// Close drains the queue and waits for running jobs. It does not cancel them:
// a pull that is halfway through 40 GiB should finish.
func (w *Worker) Close() {
	if !w.closed.CompareAndSwap(false, true) {
		return
	}
	close(w.queue)
	w.wg.Wait()
}

// NormalizeModelName accepts "owner/name" or a bare name and returns the
// canonical "owner/name". A bare name is an error rather than being invented
// into a default owner, because guessing an owner writes weights to a prefix
// nobody will look in.
func NormalizeModelName(in string) (owner, name string, err error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(in), "hf.co/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf(
		"invalid model name %q; expected owner/name as on the Hugging Face Hub", in)
}
