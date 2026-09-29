package openai

import "github.com/zlogic/fleet/core/internal/engine"

// Adapter probes endpoints that speak the OpenAI-compatible protocol. Every
// runtime Fleet supports today does, so this is the default and the only
// adapter most deployments ever need.
type Adapter struct {
	client *ProbeClient
}

func NewAdapter(c *ProbeClient) *Adapter {
	return &Adapter{client: c}
}

var _ engine.Adapter = (*Adapter)(nil)

// defaultRegistry maps every engine family to the OpenAI-compatible adapter.
// A deployment naming an engine with no dedicated adapter must still work —
// that is the whole point of P1 — so the lookup never fails.
type defaultRegistry struct{ adapter engine.Adapter }

func (r defaultRegistry) AdapterFor(string) engine.Adapter { return r.adapter }

// NewRegistry returns a Registry backed by a single OpenAI-compatible adapter.
// Registering a specialised adapter later means embedding this and overriding
// AdapterFor for that one engine name, not rewriting call sites.
func NewRegistry(c *ProbeClient) engine.Registry {
	return defaultRegistry{adapter: NewAdapter(c)}
}
