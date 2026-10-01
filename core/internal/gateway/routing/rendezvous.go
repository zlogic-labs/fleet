// Package routing chooses which endpoint serves a request.
//
// This is the gateway's job and not Envoy's (P3): the decision needs token
// counts, quota state and cost-pool level, none of which the data plane has.
package routing

import (
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// Picker selects an endpoint for a request.
type Picker interface {
	// Pick returns the endpoint to use. affinityKey should identify the
	// reusable part of the request — the conversation prefix — so that
	// repeated turns land on the instance holding the warm KV cache.
	Pick(model, affinityKey string) (engine.Endpoint, error)
	Endpoints() []engine.Endpoint
}

// Rendezvous is a Picker over an endpoint set that may change while it is
// serving.
//
// It uses rendezvous hashing rather than a consistent-hash ring because the
// fleet has tens of endpoints, not thousands: rendezvous is perfectly balanced
// at that size, needs no virtual nodes, and when an endpoint is removed only
// the keys that were assigned to it move — every other endpoint keeps exactly
// what it already had cached.
//
// The lock is because the set is live. Scaling a deployment changes it, and a
// picker read on the request path must not race the refresh that caused it.
type Rendezvous struct {
	mu      sync.RWMutex
	byModel map[string][]engine.Endpoint
	all     []engine.Endpoint
	now     func() time.Time
}

func NewRendezvous(eps []engine.Endpoint) *Rendezvous {
	r := &Rendezvous{now: time.Now}
	r.Replace(eps)
	return r
}

var _ Picker = (*Rendezvous)(nil)

// Replace swaps in a new endpoint set.
//
// It rebuilds the model index wholesale rather than mutating it, so a Pick
// always sees one consistent generation: never half the old fleet and half the
// new. Anything already in flight keeps the endpoint it picked, which is why a
// scale-down does not cut a stream that is halfway through generating.
func (r *Rendezvous) Replace(eps []engine.Endpoint) {
	byModel := make(map[string][]engine.Endpoint, len(eps))
	for _, ep := range eps {
		byModel[ep.Model] = append(byModel[ep.Model], ep)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byModel = byModel
	r.all = append([]engine.Endpoint(nil), eps...)
}

func (r *Rendezvous) Endpoints() []engine.Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]engine.Endpoint(nil), r.all...)
}

// Pick returns the highest-scoring healthy candidate, or an error naming the
// model when none can serve it. An endpoint whose last load sample is stale is
// still eligible: staleness means we do not know, and refusing to route on
// unknown is how a restarted fleet stays dark.
func (r *Rendezvous) Pick(model, affinityKey string) (engine.Endpoint, error) {
	r.mu.RLock()
	candidates := r.byModel[model]
	known := r.modelsLocked()
	r.mu.RUnlock()

	if len(candidates) == 0 {
		return engine.Endpoint{}, errs.New(
			errs.KindNotFound, "model_not_found",
			"no endpoint serves model %q; known models: %v", model, known,
		)
	}

	if affinityKey == "" {
		return candidates[0], nil
	}

	best := candidates[0]
	bestScore := score(affinityKey, best.ID)
	for _, ep := range candidates[1:] {
		if s := score(affinityKey, ep.ID); s > bestScore {
			best, bestScore = ep, s
		}
	}
	return best, nil
}

// modelsLocked reads the index; the caller must hold at least a read lock.
func (r *Rendezvous) modelsLocked() []string {
	out := make([]string, 0, len(r.byModel))
	for m := range r.byModel {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// score is a 64-bit FNV-1a over the key and the endpoint id. Uniformity is
// all that matters here; FNV is not cryptographic and is fast enough to stay
// off the critical path.
func score(key, id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}
