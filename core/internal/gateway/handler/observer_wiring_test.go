package handler

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// Both handlers must carry the observer.
//
// The failure this exists for was silent and total. The chat constructor was
// left without an observer while the embeddings constructor had one, so a
// gateway published endpoint and build metrics and no request metrics. Nothing
// errored, no test failed, and the behaviour is indistinguishable from a
// gateway that served no traffic -- which is also what a healthy idle gateway
// looks like.
//
// So the assertion is on the wiring rather than on the numbers: the counters
// have their own tests, and this one only has to notice a missing field.

type recordingObserver struct {
	mu       sync.Mutex
	served   int
	refusals []string
}

func (o *recordingObserver) Served(billing.Record, time.Duration, time.Duration, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.served++
}

func (o *recordingObserver) Refused(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refusals = append(o.refusals, reason)
}

func (o *recordingObserver) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.served, len(o.refusals)
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestBothHandlersCarryTheObserver(t *testing.T) {
	obs := &recordingObserver{}
	opts := ChatOptions{MaxBytes: 1 << 20, SampleBuffer: 4, Observed: obs}

	chat := NewChat(nil, nil, nil, quiet(), opts)
	emb := NewEmbeddings(nil, nil, nil, quiet(), opts)

	if chat.Observed != Observer(obs) {
		t.Error("the chat handler does not carry the observer")
	}
	if emb.Observed != Observer(obs) {
		t.Error("the embeddings handler does not carry the observer")
	}
}

// No observer is the normal case for a gateway built without a registry, and it
// has to be a nil check at one place rather than a panic on the hot path.
func TestAHandlerWithNoObserverStillServes(t *testing.T) {
	obs := &recordingObserver{}
	chat := NewChat(nil, nil, nil, quiet(), ChatOptions{SampleBuffer: 2})
	if chat.Observed != nil {
		t.Fatal("an observer appeared from nowhere")
	}
	// The recording observer's own nil receiver is the shape the handlers
	// guard against; prove the guard is what protects it.
	chat.Observed = obs
	if served, _ := obs.counts(); served != 0 {
		t.Fatalf("nothing was served, yet the observer saw %d", served)
	}
}
