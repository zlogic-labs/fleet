package registry

import (
	"context"
	"sync"
	"time"
)

// Memory is the in-process Store.
//
// It is the default backend, and the right one for a single control plane:
// the registry is metadata, a Fleet installation has a handful of models, and
// a pull in flight is a process the operator can see. Postgres arrives when
// there is more than one control plane, which is also when the high-availability
// capability becomes worth buying.
type Memory struct {
	mu          sync.RWMutex
	models      map[string]Model
	pulls       map[string]*Pull
	order       []string
	clusters    map[string]Cluster
	deployments map[string]Deployment
}

var _ Store = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{
		models:      map[string]Model{},
		pulls:       map[string]*Pull{},
		clusters:    map[string]Cluster{},
		deployments: map[string]Deployment{},
	}
}

func (m *Memory) UpsertModel(_ context.Context, in Model) (Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	prev, existed := m.models[in.Name]
	out := in
	if existed {
		out.CreatedAt = prev.CreatedAt
		// A partial update must not blank fields it does not mention. Every
		// caller here updates a subset, so a zero means "not supplied" and the
		// previous value stands — including State, because silently un-readying
		// a model whose weights are already stored would force another pull.
		// Zero is safe to treat as absent for all of these: a model with a
		// context limit of 0 or a size of 0 bytes is not a thing that exists.
		inherit(&out.State, prev.State)
		inherit(&out.SizeBytes, prev.SizeBytes)
		inherit(&out.Files, prev.Files)
		inherit(&out.Prefix, prev.Prefix)
		inherit(&out.Commit, prev.Commit)
		inherit(&out.Tokenizer, prev.Tokenizer)
		inherit(&out.Context, prev.Context)
		inherit(&out.Format, prev.Format)
	}
	out.UpdatedAt = now
	if out.CreatedAt.IsZero() {
		out.CreatedAt = now
	}
	m.models[in.Name] = out
	return out, nil
}

func (m *Memory) GetModel(_ context.Context, name string) (Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mdl, ok := m.models[name]
	if !ok {
		return Model{}, ErrNotFound
	}
	return mdl, nil
}

func (m *Memory) ListModels(_ context.Context) ([]Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Model, 0, len(m.models))
	for _, mdl := range m.models {
		out = append(out, mdl)
	}
	sortModels(out)
	return out, nil
}

func (m *Memory) DeleteModel(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.models[name]; !ok {
		return ErrNotFound
	}
	// The objects stay. A registry entry is a pointer at weights, and deleting
	// a pointer is cheap while deleting 140 GiB on a mis-click is not.
	delete(m.models, name)
	return nil
}

func (m *Memory) ReportCluster(_ context.Context, c Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.ReportedAt = time.Now().UTC()
	// Recomputed here rather than in the operator, so a hand-written or
	// replayed report cannot lie about its own totals.
	c.Recompute()
	m.clusters[c.Name] = c
	return nil
}

func (m *Memory) ListClusters(_ context.Context) ([]Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c)
	}
	sortClusters(out)
	return out, nil
}

func (m *Memory) UpsertDeployment(_ context.Context, d Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.ObservedAt = time.Now().UTC()
	m.deployments[d.Key()] = d
	return nil
}

func (m *Memory) ListDeployments(_ context.Context) ([]Deployment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Deployment, 0, len(m.deployments))
	for _, d := range m.deployments {
		out = append(out, d)
	}
	sortDeployments(out)
	return out, nil
}

// inherit keeps a previous value when the new one is the zero value.
func inherit[T comparable](dst *T, prev T) {
	var zero T
	if *dst == zero {
		*dst = prev
	}
}
