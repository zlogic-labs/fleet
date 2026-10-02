package billing

import (
	"context"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// The settled record: one request, priced, ready to be stored.
//
// It lives here rather than in the store package because it is a domain fact —
// what one request cost and who spent it — and both sides need it. The gateway
// produces it; PostgreSQL persists it. Putting the type in either one would
// force the other to import it, and the handler has no business importing a
// database driver.

// Record is one completed request as the ledger sees it.
//
// Every billing dimension is a field rather than a lookup, per the schema
// header: a key's project assignment may change, and a report that resolved it
// at read time would attribute last month's usage to whichever project the key
// points at now.
type Record struct {
	Tenant  string
	Project string
	KeyID   string
	// Model is the resolved model the endpoint served, not the string the
	// client sent. An alias re-pointed to a cheaper model must not be able to
	// rewrite last month's bill.
	Model    string
	Endpoint string
	// PriceBook names the book that was applied, for audit. It is a reference
	// and is not used to re-price: Amount below is the figure that was charged.
	PriceBook string

	// Usage is the engine's own report (P6). When UsageKnown is false this is
	// the estimate the charge was based on, and the row says so so a
	// reconciliation pass can find it rather than it being invisible inside an
	// aggregate.
	Usage      openai.Usage
	UsageKnown bool
	Amount     Amount

	TTFT       time.Duration
	Duration   time.Duration
	Streamed   bool
	OccurredAt time.Time
}

// Recorder persists a settled record.
//
// Append-only, and the interface has no update or delete because the ledger has
// neither. A correction is a new record that references the one it corrects,
// which is what makes an invoice reproducible from the table alone.
//
// Record returns the stored row's id, so a dispute or a refund can point at one
// row rather than at a time range.
type Recorder interface {
	Record(ctx context.Context, r Record) (int64, error)
}

// PricerSource supplies prices at request time.
//
// Not the same as Pricer, which is the arithmetic over one book in memory. A
// source is where the book comes from and whether it is current — a database
// that may have been reloaded since the last request. Keeping them apart is
// what lets the settlement path be tested against a fixed price and a real
// deployment keep its prices fresh.
//
// BookID is part of the same contract rather than a separate lookup because the
// two must agree: a record quoting a price book its charge did not come from is
// an audit trail that lies.
//
// An error from Charge means "no price for this model", never "the price is
// zero". A tenant with an unbilled model is a tenant nobody notices is being
// given a GPU for free.
type PricerSource interface {
	// Charge takes a context because a source may have to read: a price book
	// that lives in a database needs the caller's deadline, or a stalled
	// database would hold the request open past the point where anybody is
	// still waiting for the answer.
	Charge(ctx context.Context, model string, u openai.Usage) (Amount, error)
	// BookID names the book in force for a model, or "" when it has none.
	BookID(model string) string
}
