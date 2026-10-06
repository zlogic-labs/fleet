package handler

import (
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// The provider label, read off an endpoint.
//
// A label rather than a typed field on engine.Endpoint because a provider is
// what a *deployment* is, not what an endpoint is: the same provider serves a
// dozen endpoints, and the label travels with the inventory contract and the
// gateway's own upstream configuration without either of them needing to know
// about billing.
//
// The label name is part of the inventory contract rather than a private
// convention. fleet-serving writes it, the gateway reads it, and a deployment
// that leaves it out is a self-hosted one — which is every deployment that
// existed before this label did.

// providerLabel is the endpoint label naming who served the model.
const providerLabel = "provider"

// providerOf returns the provider that served a request, normalised.
//
// Empty means the fleet's own engines, which is billing.CentrePool. The
// fallback to empty for an endpoint with no label at all is deliberate: every
// deployment that predates the label, and every self-hosted deployment an
// operator configures by hand, must keep working, and it must keep being
// *billed as fleet capacity*. Reading "unknown" as a vendor would move a
// deployment's money into a column whose provider nobody can name.
func providerOf(ep engine.Endpoint) billing.Provider {
	return billing.NormalizeProvider(ep.Labels[providerLabel])
}
