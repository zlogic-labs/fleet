package registry

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The store keeps its own copy of a pull.
//
// The worker's job is mutated for the length of a download without the store's
// lock held, and the console polls the stored one. Handing the caller's pointer
// to the store made those two the same object, so every poll read fields the
// worker was writing. Asserting on the pointer rather than on a race keeps the
// test deterministic: -race is not runnable everywhere, and the failure this
// guards against is a data race that a flaky stress loop would find eventually
// or never.
func TestTheStoreDoesNotShareTheCallersPull(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	caller := &Pull{ID: "p1", Model: "acme/research", SourceRef: "acme/research", State: PullQueued}
	if err := m.CreatePull(ctx, caller); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}

	caller.State = PullRunning
	caller.Progress = 0.5
	caller.BytesDone = 512

	got, err := m.GetPull(ctx, "p1")
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got.State != PullQueued || got.Progress != 0 || got.BytesDone != 0 {
		t.Errorf("the stored job followed the caller's edits: %+v", got)
	}
}

// Every field the console reads while a pull runs has to reach the stored job.
// Writing only to the worker's copy froze the console at zero for the whole
// download, which reads as a hung pull rather than as a missing publish.
func TestProgressReachesTheStoredJob(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	if err := m.CreatePull(ctx, &Pull{ID: "p1"}); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if _, err := m.UpdatePull(ctx, "p1", func(p *Pull) {
		p.BytesDone, p.FilesDone, p.CurrentFile, p.Progress = 700, 3, "b.safetensors", 0.7
	}); err != nil {
		t.Fatalf("UpdatePull: %v", err)
	}

	got, err := m.GetPull(ctx, "p1")
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got.BytesDone != 700 || got.FilesDone != 3 || got.Progress != 0.7 || got.CurrentFile != "b.safetensors" {
		t.Errorf("progress did not reach the store: %+v", got)
	}
}

// A second Finish must not rewrite history. A worker that reports failure after
// a cancellation used to turn a finished job into a failed one, and the
// failure message is what an operator reads to decide whether to retry.
func TestFinishIsIdempotentForTheWholeJob(t *testing.T) {
	p := &Pull{ID: "p1"}
	p.Finish(PullCanceled, "canceled by operator")

	first := *p
	p.Finish(PullFailed, "a late failure report")

	if p.State != first.State || p.Error != first.Error {
		t.Errorf("a second Finish rewrote the job: %+v, want %+v", p, first)
	}
	if !p.FinishedAt.Equal(first.FinishedAt.UTC()) {
		t.Errorf("FinishedAt moved: %v, want %v", p.FinishedAt, first.FinishedAt)
	}
}

// Waiting on Done has to work from a stored copy, since that is the copy a
// caller has. The channel is shared between the store's object and every clone
// on purpose: copying it would hand every waiter a channel nobody closes.
func TestTheStoredJobStillSignalsItsWaiters(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.CreatePull(ctx, &Pull{ID: "p1"}); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}

	woke := make(chan struct{})
	go func() {
		defer close(woke)
		got, err := m.GetPull(ctx, "p1")
		if err != nil {
			return
		}
		<-got.Done()
	}()

	time.Sleep(20 * time.Millisecond)
	stored, err := m.GetPull(ctx, "p1")
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	stored.Finish(PullDone, "")

	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never woke: the clone broke the shared channel")
	}
}

// Concurrent publishers against concurrent readers: under -race this is the
// test that reports the sharing bug as a race rather than as a wrong number.
// Without -race it still has to pass, which is why UpdatePull takes the write
// lock and the readers take the read lock.
func TestReadersAndWritersDoNotCorruptEachOther(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.CreatePull(ctx, &Pull{ID: "p1"}); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = m.UpdatePull(ctx, "p1", func(p *Pull) { p.Progress = float64(i) / 200 })
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := m.ListPulls(ctx); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	got, err := m.GetPull(ctx, "p1")
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got.Progress < 0.99 {
		t.Errorf("progress = %v, want the last write to have landed", got.Progress)
	}
}
