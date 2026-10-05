package openai

import (
	"context"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// Adapter probes endpoints that speak the OpenAI-compatible protocol. Every
// runtime Fleet supports today does, so this is the only probe there is.
//
// It is a concrete type rather than an implementation of an interface. There
// used to be an engine.Adapter interface, and it promised a second
// implementation that P1 forbids: a second engine is a second Profile, which is
// a literal. An interface with one implementation and no substituter is a
// promise nobody has checked.
//
// The caller is fleet-serving, not the gateway. What the gateway knows about an
// engine arrives through the inventory contract.
type Adapter struct {
	client *ProbeClient
}

func NewAdapter(c *ProbeClient) *Adapter { return &Adapter{client: c} }

// NewProfiles returns the profile registry Fleet ships with.
//
// There is no adapter registry any more, and that removal is the point. Under
// the old shape a second engine tempted a second Adapter, which is exactly the
// vendor coupling P1 forbids. Now a second engine is a second Profile, which is
// a literal.
func NewProfiles() *engine.Profiles { return engine.BuiltinProfiles() }

// compile-time proof that the probe signature is the one fleet-serving expects,
// without an interface to promise polymorphism that will not arrive.
var _ = func(ctx context.Context, ep engine.Endpoint, p engine.Profile) (engine.Capability, error) {
	return (&Adapter{}).Probe(ctx, ep, p)
}
