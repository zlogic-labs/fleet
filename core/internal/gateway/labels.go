package gateway

import "github.com/zlogic-labs/fleet/core/pkg/billing"

// projectName is the bare project name for a metric label.
//
// The ledger row carries the qualified id, "acme/research", because that is the
// key its foreign relationships use. A label is not a key: the tenant label
// already carries the scope, so a project label of "acme/research" under
// tenant="acme" repeats the tenant and makes every per-project query a string
// prefix match instead of an equality.
func projectName(rec billing.Record) string {
	if rec.Tenant == "" {
		return rec.Project
	}
	if prefix := rec.Tenant + "/"; len(rec.Project) > len(prefix) &&
		rec.Project[:len(prefix)] == prefix {
		return rec.Project[len(prefix):]
	}
	return rec.Project
}
