// Package routing chooses which endpoint serves a request.
//
// This is the gateway's job and not Envoy's (P3): the decision needs token
// counts, quota state and cost-pool level, none of which the data plane has.
package routing

import (
	"hash/fnv"
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

// Rendezvous is a Picker over a static endpoint set.
//
// It uses rendezvous hashing rather than a consistent-hash ring because the
// fleet has tens of endpoints, not thousands: rendezvous is perfectly balanced
// at that size, needs no virtual nodes, and when an endpoint is removed only
// the keys that were assigned to it move — every other endpoint keeps exactly
// what it already had cached.
type Rendezvous struct {
	byModel map[string][]engine.Endpoint
	all     []engine.Endpoint
	now     func() time.Time
}

func NewRendezvous(eps []engine.Endpoint) *Rendezvous {
	r := &Rendezvous{
		byModel: make(map[string][]engine.Endpoint),
		all:     append([]engine.Endpoint(nil), eps...),
		now:     time.Now,
	}
	for _, ep := range eps {
		r.byModel[ep.Model] = append(r.byModel[ep.Model], ep)
	}
	return r
}

var _ Picker = (*Rendezvous)(nil)

func (r *Rendezvous) Endpoints() []engine.Endpoint { return r.all }

// Pick returns the highest-scoring healthy candidate, or an error naming the
// model when none can serve it. An endpoint whose last load sample is stale is
// still eligible: staleness means we do not know, and refusing to route on
// unknown is how a restarted fleet stays dark.
func (r *Rendezvous) Pick(model, affinityKey string) (engine.Endpoint, error) {
	candidates := r.byModel[model]
	if len(candidates) == 0 {
		return engine.Endpoint{}, errs.New(
			errs.KindNotFound, "model_not_found",
			"no endpoint serves model %q; known models: %v", model, r.models(),
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

func (r *Rendezvous) models() []string {
	out := make([]string, 0, len(r.byModel))
	for m := range r.byModel {
		out = append(out, m)
	}
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
