package ratelimit

import "time"

// Reservation is a committed deduction awaiting settlement.
//
// It is a value rather than a token because the caller holds exactly one and
// passes it back by value; the shared state lives in the limiter's own store,
// keyed by an opaque id. That keeps the thing that crosses package boundaries
// trivially copyable, and keeps the lock out of every caller's struct.
//
// It names two buckets rather than one because a reservation is taken against
// both the tenant envelope and the project partition. Settlement has to release
// both, and it can only do that if it remembers both — which is also why the
// two are settled together rather than one being an optimisation of the other.
type Reservation struct {
	id     string
	tenant string
	// project is the partition charged, empty when the request was charged to
	// the tenant envelope alone.
	project string
	// Reserved is what was deducted. Settlement removes all of it and records
	// the actual amount instead.
	Reserved int
	// counted records whether this reservation took a request slot. It is false
	// when both policies are unlimited, which returns before touching any
	// counter, and settlement must not then credit back a request that was
	// never counted.
	counted bool
	// at is the unix second the deduction was recorded in, so settlement
	// releases from the row it was charged to.
	//
	// A limiter that keeps counters in process walks the ring backwards to find
	// the deduction. One that keeps them in a database can name the row, which
	// is both cheaper and more accurate: the refund leaves the window at the
	// same moment the charge would have, instead of at whatever second held the
	// oldest tokens it could find.
	at int64
}

// NewReservation builds a reservation for a limiter whose counters live
// outside this process.
//
// Exported because Memory sets these fields directly and a database-backed
// Limiter cannot: the fields are unexported on purpose, because a caller that
// invents a reservation holding neither the scope it was charged to nor the
// second it was charged in cannot have its settlement applied correctly.
func NewReservation(id, tenant, project string, reserved int, at time.Time) Reservation {
	return Reservation{
		id:       id,
		tenant:   tenant,
		project:  project,
		Reserved: reserved,
		counted:  true,
		at:       at.Unix(),
	}
}

// Admitted is a reservation that was allowed through and charged nothing,
// because no limit applied. It still carries the scope, so a caller reporting
// on it names the right thing, and its settlement is a no-op rather than a
// credit against a counter that was never incremented.
func Admitted(tenant, project string) Reservation {
	return Reservation{tenant: tenant, project: project}
}

// ID identifies the reservation for logs.
func (r Reservation) ID() string { return r.id }

// Second is the unix second the deduction was recorded in. A Limiter that
// stores counters elsewhere needs it to refund the row it charged.
func (r Reservation) Second() int64 { return r.at }

// WasCounted reports whether this reservation took a request slot. A settlement
// that credited back against a reservation which never charged anything would
// hand the scope free capacity.
func (r Reservation) WasCounted() bool { return r.counted }

// TenantName is who the reservation is charged to.
func (r Reservation) TenantName() string { return r.tenant }

// ProjectName is which partition was also charged, or "" for the envelope only.
func (r Reservation) ProjectName() string { return r.project }
