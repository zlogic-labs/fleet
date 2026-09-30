package openai

import "github.com/zlogic-labs/fleet/core/pkg/engine"

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

// NewProfiles returns the profile registry Fleet ships with.
//
// There is no adapter registry any more, and that removal is the point. Under
// the old shape a second engine tempted a second Adapter, which is exactly the
// vendor coupling P1 forbids. Now a second engine is a second Profile, which is
// a literal, and the Adapter count stays at one per protocol.
func NewProfiles() *engine.Profiles { return engine.BuiltinProfiles() }
