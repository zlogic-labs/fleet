package postgres

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// scanDetailRow turns one ledger row into the record the mirror stores.
//
// It exists because the ledger's row and the mirror's row are not the same shape:
// the ledger stores cached and reasoning tokens as columns that may be absent
// (a NULL means the engine reported no breakdown at all, which is different from
// zero), while the mirror stores them as plain integers. Deciding that difference
// here, once, keeps the backfill and the live writer from disagreeing about what
// a NULL meant -- and two code paths that interpret the same column differently
// is how a mirror ends up disagreeing with its own source.
func scanDetailRow(rows pgx.Rows) (billing.Record, error) {
	var (
		r         billing.Record
		project   *string
		keyID     *string
		priceBook *string
		cached    *int64
		reasoning *int64
		source    string
		amount    int64
	)
	err := rows.Scan(
		&r.LedgerID, &r.Tenant, &project, &keyID, &r.Model, &r.Endpoint, &priceBook,
		&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &cached, &reasoning,
		&amount, &r.UsageKnown, &source, &r.Truncated,
		&r.TTFT, &r.Duration, &r.Streamed, &r.OccurredAt,
	)
	if err != nil {
		return r, fmt.Errorf("postgres: reading a usage row for the detail store: %w", err)
	}

	// These three are written as NULL rather than as an empty string, because an
	// empty project or price book is not the same claim as a named one.
	//
	// Reading them as plain strings is not a detail: it fails on the first
	// anonymous request and on the first model nobody has priced, which is most
	// of a fresh install. The backfill would then abort and the mirror would stay
	// empty for exactly the deployments that need it rebuilt.
	r.Project = orEmpty(project)
	r.KeyID = orEmpty(keyID)
	r.PriceBook = orEmpty(priceBook)

	r.Amount = billing.Amount(amount)
	r.UsageSource = billing.Source(source)
	if r.UsageSource == "" {
		r.UsageSource = billing.SourceEngine
	}
	if cached != nil {
		r.Usage.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: int(*cached)}
	}
	if reasoning != nil {
		r.Usage.CompletionTokensDetails = &openai.CompletionTokensDetails{
			ReasoningTokens: int(*reasoning),
		}
	}
	// Durations are stored as milliseconds; a caller that reconstructs the
	// record expects a duration. Going through milliseconds rather than
	// nanoseconds is the point: a ledger row is accurate to a millisecond and
	// pretending otherwise would make the mirror's figures differ from the
	// ledger's in the last digits.
	r.TTFT = time.Duration(r.TTFT) * time.Millisecond
	r.Duration = time.Duration(r.Duration) * time.Millisecond
	return r, nil
}

// orEmpty turns a NULL text column into the empty string the record uses.
//
// One place so the ledger's writer (which writes NULL through nullIfEmpty) and
// this reader agree on what absent means.
func orEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
