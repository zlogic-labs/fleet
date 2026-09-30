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

	"github.com/zlogic-labs/fleet/core/pkg/errs"
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

	// Numeric limits. Zero means unlimited, so the community default does not
	// need to pick an arbitrary ceiling.
	MaxNodes int
	MaxGPUs  int
	MaxUsers int
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

// Allow reports whether a numeric limit has room. A limit of zero is unlimited.
func (l License) Allow(limit int, current int) bool {
	return limit == 0 || current < limit
}

// Require returns a typed error when the capability is not granted. Handlers
// call it and let errs decide the status, so no handler has to know the
// edition rules.
func (l License) Require(c Capability, now time.Time) error {
	if l.Granted(c, now) {
		return nil
	}
	return errs.New(
		errs.KindPermissionDenied,
		"feature_requires_enterprise",
		"%s is an enterprise capability; this deployment runs the %s edition",
		c, l.Edition,
	)
}

// RequireLimit is the numeric counterpart of Require.
func (l License) RequireLimit(name string, limit, current int) error {
	if l.Allow(limit, current) {
		return nil
	}
	return errs.New(
		errs.KindPermissionDenied,
		"quota_exceeded",
		"this deployment is limited to %d %s; %d are in use",
		limit, name, current,
	)
}

// Checker is what handlers depend on rather than a concrete License, so that
// tests can substitute one without touching a clock or a file.
type Checker interface {
	License() License
	Has(Capability) bool
}

// Static is a Checker over a fixed License. It is the community build's
// implementation and the fallback everywhere else.
type Static struct{ lic License }

func NewStatic(l License) Static { return Static{lic: l} }

func (s Static) License() License      { return s.lic }
func (s Static) Has(c Capability) bool { return s.lic.Granted(c, time.Now()) }
