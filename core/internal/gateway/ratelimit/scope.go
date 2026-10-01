package ratelimit

import "strings"

// Scope is who a request is charged to.
//
// Two levels, and a request is checked against both. That is the whole point
// of the type: a per-project limit on its own is arithmetic a tenant can
// defeat, because a tenant allowed to create projects would multiply its own
// ceiling by the number of projects it created. Ten projects at 300 requests
// per minute is 3000, and no per-project number would ever have bound. The
// tenant envelope is what makes the partitions add up to something the
// platform sized its GPUs for; the project limits are how the envelope is
// divided, not how it is grown.
//
// It is two names rather than a path because the two answers come from
// different places and mean different things: the tenant is who pays, the
// project is what the spend is attributed to.
type Scope struct {
	// Tenant is who is charged. Several projects and several keys share it.
	Tenant string
	// Project is the partition inside the tenant. Empty means the request is
	// charged to the tenant envelope alone, with no partition of its own.
	Project string
}

// Normalized puts a scope in the canonical spelling the buckets are keyed on.
//
// Both halves, and the project is dropped when there is no tenant: a project
// with no tenant names a partition of nothing, and keying it anyway would hand
// out an allowance nothing else could spend. Configuration and key specs
// cannot produce one — validate rejects a project limit with no tenant, and
// ParsePrincipal requires both segments — so this is a backstop, not a path.
func (s Scope) Normalized() Scope {
	return Scope{
		Tenant:  strings.ToLower(strings.TrimSpace(s.Tenant)),
		Project: strings.ToLower(strings.TrimSpace(s.Project)),
	}.withoutOrphan()
}

func (s Scope) withoutOrphan() Scope {
	if s.Tenant == "" {
		s.Project = ""
	}
	return s
}

// Key is the bucket a scope's counter lives in.
//
// Three shapes, and they cannot collide: "" for nobody, a bare tenant for an
// envelope, and "tenant/project" for a partition. The last cannot be produced
// by a tenant name, because a tenant name never contains a slash — ParsePrincipal
// splits on it, so the two-segment spelling is unreachable for a tenant.
func (s Scope) Key() string {
	if s.Tenant == "" {
		return ""
	}
	if s.Project == "" {
		return s.Tenant
	}
	return s.Tenant + "/" + s.Project
}

// TenantKey is the bucket holding the envelope, whether or not the scope also
// names a project. Every request touches this one.
func (s Scope) TenantKey() string { return Scope{Tenant: s.Tenant}.Key() }

// String names the scope for a log line or an error message.
func (s Scope) String() string {
	if s.Project == "" {
		return s.Tenant
	}
	return s.Tenant + "/" + s.Project
}

// Anonymous is the scope of a request that authenticated to nobody.
//
// Its own value rather than a zero Scope, so a caller cannot accidentally
// construct the anonymous bucket by leaving a field unset and then spend from
// it. The limiter treats it as unlimited, which is the honest reading: nothing
// is being charged for it, so there is nothing to ration.
var Anonymous = Scope{}
