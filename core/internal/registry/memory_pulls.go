package registry

import (
	"context"
	"errors"
	"time"
)

// The pull half of the in-process store. Models, clusters and deployments are
// plain values written by one reporter; pulls are jobs with a lifecycle, a
// progress figure the console polls continuously, and a cancel function that
// has to reach a worker holding its own copy.

func (m *Memory) CreatePull(_ context.Context, p *Pull) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pulls[p.ID]; ok {
		return errors.New("registry: pull already exists: " + p.ID)
	}
	if p.done == nil {
		p.done = make(chan struct{})
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = time.Now().UTC()
	}
	// A copy, not the caller's pointer. Enqueue hands the same job to a worker
	// that then mutates it for the length of the download, and a reader under
	// RLock would be reading those writes. Storing the pointer made clone()
	// describe a protection it did not provide.
	//
	// The copy shares the done channel, so a worker closing it still releases
	// whoever is waiting on the stored job.
	m.pulls[p.ID] = p.clone()
	m.order = append(m.order, p.ID)
	return nil
}

func (m *Memory) UpdatePull(_ context.Context, id string, fn func(*Pull)) (*Pull, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pulls[id]
	if !ok {
		return nil, ErrNotFound
	}
	fn(p)
	return p, nil
}

func (m *Memory) GetPull(_ context.Context, id string) (*Pull, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pulls[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p.clone(), nil
}

func (m *Memory) ListPulls(_ context.Context) ([]*Pull, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Pull, 0, len(m.pulls))
	for _, id := range m.order {
		if p, ok := m.pulls[id]; ok {
			out = append(out, p.clone())
		}
	}
	return out, nil
}

// CancelPull returns false for a job that is not running, which is how the API
// distinguishes "already finished" from "no such job".
func (m *Memory) CancelPull(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pulls[id]
	if !ok {
		return false, ErrNotFound
	}
	if !p.State.Running() {
		return false, nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	return true, nil
}

// clone copies a pull for a reader. The live job is mutated by its worker
// through UpdatePull rather than in place, so a reader under RLock sees whole
// states -- but handing out the pointer would still let a caller hold a
// reference into the store and mutate it without the write lock.
func (p *Pull) clone() *Pull {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// AttachCancel wires the worker's cancel function to the worker's own job.
//
// That is not enough on its own: the store keeps a separate copy, so CancelPull
// reads a cancel function that was never set and reports that it stopped a job
// it did not stop. Worker.runOne publishes the same function through
// UpdatePull for that reason, and both lines are needed.
func (p *Pull) AttachCancel(fn context.CancelFunc) { p.cancel = fn }

// Finish releases anyone waiting on Done and stamps the end time.
//
// Idempotent for the whole job, not just for the close. p.done is nil'd after
// the first close, so a second call cannot panic -- but without the
// FinishedAt guard it would overwrite State, Error and FinishedAt, and a caller
// reporting a failure after a cancellation would rewrite a finished job as
// failed. The error string is what an operator reads to decide whether to
// retry.
func (p *Pull) Finish(state PullState, errMsg string) {
	if p.FinishedAt != nil {
		return
	}
	p.State = state
	p.Error = errMsg
	t := time.Now().UTC()
	p.FinishedAt = &t
	if state == PullDone {
		p.Progress = 1
		p.BytesDone = p.BytesTotal
		p.FilesDone = p.FilesTotal
	}
	if p.done != nil {
		close(p.done)
		p.done = nil
	}
}
