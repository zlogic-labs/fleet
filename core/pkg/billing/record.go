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
	// LedgerID is the authoritative row id, once the ledger has assigned one.
	//
	// It is zero before the insert and filled afterwards, which is why the
	// detail mirror carries it separately from the record's content: a
	// reconciliation compares two stores by identity, and matching on content
	// would report a difference whenever two identical requests happened.
	LedgerID int64

	Tenant  string
	Project string
	KeyID   string
	// Model is the resolved model the endpoint served, not the string the
	// client sent. An alias re-pointed to a cheaper model must not be able to
	// rewrite last month's bill.
	Model    string
	Endpoint string
	// Provider names who served the request, empty for the fleet's own engines.
	//
	// Recorded per row rather than joined from the endpoint, for the reason the
	// tenant and project are: an endpoint is repointed. A gateway that moves a
	// model to a vendor in April would, without this column, make March look
	// like April's traffic — and March is a closed invoice.
	//
	// It also decides which centre the money reaches. See Centre.
	Provider Provider
	// PriceBook names the book that was applied, for audit. It is a reference
	// and is not used to re-price: Amount below is the figure that was charged.
	PriceBook string

	// Usage is the tokens this request was charged for. Which of the three
	// available answers it is, is UsageSource; the two are separate because
	// "the engine reported nothing" and "the gateway counted the answer" are
	// different facts about different situations, and a report that wants to
	// ask how much of the fleet's token volume was measured rather than
	// assumed needs to be able to tell them apart.
	Usage      openai.Usage
	UsageKnown bool
	// UsageSource names where Usage came from.
	UsageSource Source
	// Truncated says a counted answer was longer than the gateway keeps, so
	// the completion figure is a floor rather than the whole output.
	//
	// It is recorded rather than inferred because nothing downstream can
	// recover it: the row looks identical to a short answer otherwise, and a
	// floor quietly averaged into a total is the exact failure the agreement
	// check exists to prevent.
	Truncated bool
	Amount    Amount

	TTFT       time.Duration
	Duration   time.Duration
	Streamed   bool
	OccurredAt time.Time

	// Engine is what the engine itself said about this request's timing, as
	// opposed to what the gateway measured by watching the bytes go past. It is
	// nil when the engine published no timings, which is the normal case for
	// engines whose server was not started to publish them.
	//
	// The field is worth its column because of QueueMS. Time to first token
	// is queueing plus prefill plus network, and the gateway can measure the
	// sum but not the parts; when an engine separates them, the difference is
	// the only thing that distinguishes "the fleet is short of capacity" from
	// "the prompt was long", which are the same symptom and opposite
	// remedies.
	Engine *EngineTimings
}

// EngineTimings are an engine's own figures for one request, in milliseconds.
//
// Pointers throughout, and that is the load-bearing decision. Every engine in
// practice answers with null for fields it did not measure — vLLM leaves all of
// them null unless the server runs with per-request metrics enabled, and
// llama-server omits a field whose divisor it does not know. A zero here would
// be read as "no time was spent waiting", which is the single most reassuring
// wrong number available and the one an operator is most likely to act on.
type EngineTimings struct {
	QueueMS  *float64
	TTFTMS   *float64
	DecodeMS *float64
}

// Source is where a settled request's token count came from.
//
// Three values because there are three situations, and collapsing them loses
// the only thing an operator can act on: a fleet whose output tokens are mostly
// counted rather than reported is a fleet whose invoices are mostly estimates,
// and that is a fact about the engines deployed, not about the tenants using
// them.
type Source string

const (
	// SourceEngine: the engine reported usage and Fleet charged exactly that.
	SourceEngine Source = "engine"
	// SourceCounted: the engine reported nothing, so Fleet counted the answer
	// it forwarded. A measurement of the payload, not a claim about the
	// engine's internals, and wrong by a percent rather than by the ratio
	// between the requested maximum and the actual answer.
	SourceCounted Source = "counted"
	// SourceReserved: nothing could be counted — a response with no text at
	// all, or one cut short before any frame arrived — so the request was
	// charged at what it reserved. The worst of the three and rare; it is
	// named so that "how often do we bill a guess" has an answer.
	SourceReserved Source = "reserved"
)

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
// An error from Charge means "no price for this model", never "the price is
// zero". A tenant with an unbilled model is a tenant nobody notices is being
// given a GPU for free.
type PricerSource interface {
	// Charge takes a context because a source may have to read: a price book
	// that lives in a database needs the caller's deadline, or a stalled
	// database would hold the request open past the point where anybody is
	// still waiting for the answer.
	Charge(ctx context.Context, model string, provider Provider, u openai.Usage) (Amount, error)
	// BookID names the book in force for a model from a provider, or "" when it
	// has none.
	BookID(model string, provider Provider) string
	// Predict is what a request that has not run yet may cost, in tokens as
	// well as money. A budget reserves against it, and a budget can be stated
	// in tokens, so the token breakdown has to come out of the same place as
	// the price rather than being recomputed by each caller.
	//
	// Known is false when the model has no price: the figures are then a floor
	// rate from the same cost centre rather than this model's own, which is an
	// over-estimate on purpose. Reserving nothing for an unpriced model would
	// leave the budget unenforced for exactly the models nobody has priced yet.
	Predict(ctx context.Context, model string, provider Provider, promptTokens, maxTokens int) (Prediction, error)
}

// Prediction is an upper bound on one request's cost, in every dimension.
type Prediction struct {
	Usage  openai.Usage
	Amount Amount
	// Known is false when the model had no price and these figures come from
	// the cheapest rate in force in the same cost centre instead.
	Known bool
	// Model the prediction was made for, which is what a caller must record:
	// the resolved name, never the string the client sent.
	Model string
	// Provider the prediction was made against, normalised. A caller must
	// record it too, or the ledger cannot tell a vendor charge from a share of
	// the fleet's own cost — and the two must never be added together.
	Provider Provider
}
