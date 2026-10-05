// Package entitlement models the boundary between the community edition and
// the commercial one.
//
// The rule this package exists to enforce: the enterprise build must never
// require a fork of the community build. So every gated capability is
// expressed as data checked here, and as interfaces whose community
// implementations live in this repository. The enterprise module supplies
// alternative implementations and a different License; it does not contain a
// modified copy of anything below.
//
// See docs/architecture.md section 12.
package entitlement

import (
	"time"
)

// Edition is which build is running.
type Edition string

const (
	EditionCommunity  Edition = "community"
	EditionEnterprise Edition = "enterprise"
)

// Capability is a feature that may be gated. These are deliberately coarse:
// each one maps to something an enterprise buyer can be told is missing, and
// to a single place in the code where the check happens.
type Capability string

const (
	// CapSSO federated or directory-backed login (OIDC, LDAP, SAML).
	CapSSO Capability = "sso"
	// CapAudit an immutable audit trail exportable to an external sink.
	CapAudit Capability = "audit"
	// CapRBAC per-role permissions beyond the single admin/user split.
	CapRBAC Capability = "rbac"
	// CapPolicy request-level enforcement: per-tenant prompt and route policy.
	CapPolicy Capability = "policy"
	// CapMultiCluster control more than one cluster from one control plane.
	CapMultiCluster Capability = "multicluster"
	// CapHA replicated control plane and gateway with leader election.
	CapHA Capability = "ha"
	// CapCostExport scheduled cost export for finance systems.
	CapCostExport Capability = "cost_export"
)

// AllCapabilities is the closed set, used to validate a licence file and to
// drive the console's upgrade page.
var AllCapabilities = []Capability{
	CapSSO, CapAudit, CapRBAC, CapPolicy,
	CapMultiCluster, CapHA, CapCostExport,
}

// License is the entitlement set for one deployment. The community build
// constructs one of these with no capabilities set; the enterprise build reads
// it from a signed licence file.
type License struct {
	Edition   Edition
	Customer  string
	ExpiresAt time.Time

	Capabilities map[Capability]bool
}

// Community returns the licence for an unlicensed build: no gated capability,
// no numeric ceiling. An operator who outgrows it has a reason to buy, which
// is the entire point.
func Community() License {
	return License{Edition: EditionCommunity}
}

// Enterprise returns a licence granting every capability, for a build that was
// compiled with the enterprise modules. Actual entitlements still come from a
// licence file; this is the ceiling, not the grant.
func Enterprise(customer string, expires time.Time) License {
	granted := make(map[Capability]bool, len(AllCapabilities))
	for _, c := range AllCapabilities {
		granted[c] = true
	}
	return License{
		Edition:      EditionEnterprise,
		Customer:     customer,
		ExpiresAt:    expires,
		Capabilities: granted,
	}
}

// Expired reports whether the licence has lapsed. A lapsed licence falls back
// to community behaviour rather than to a hard failure, so a customer is never
// locked out of their own data by an expired invoice.
func (l License) Expired(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt)
}

// Granted reports whether a capability is available, treating an expired
// licence as community edition.
func (l License) Granted(c Capability, now time.Time) bool {
	if l.Edition != EditionEnterprise || l.Expired(now) {
		return false
	}
	return l.Capabilities[c]
}

// There is no numeric limit here and no Checker interface, and that is also a
// decision. A limit with nothing counting against it is a constant; a Checker
// with one implementation and no caller is a promise about a substitution
// nobody has needed. When a limit is required, the thing being limited will
// say what "current" means for it -- which is the only part that is hard.
