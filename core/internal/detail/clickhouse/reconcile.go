package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The replica's side of the reconciliation.
//
// Two queries rather than one, because the totals and the groups are not the
// same measurement. A GROUP BY cannot report rows whose grouping key is empty,
// so a replica that lost exactly the unattributed rows would reconcile clean
// against a ledger that still holds them — which is the shape of failure this
// whole comparison exists to catch, not a rounding difference.

// tallyColumns is the five figures both queries read, spelled once so they
// cannot drift into counting different things.
//
// Every cast is explicit. count() is UInt64 and the sums of the nullable
// breakdown are Nullable(Int64), and the driver scans by the server's type
// rather than by the Go variable's: a scan that is one type off fails the whole
// report rather than converting.
//
// The COALESCE on cached_tokens is not the same fix the ledger's queries carry,
// and the difference is worth knowing. Both servers return NULL for a sum over a
// group where every row omitted the breakdown -- measured, not assumed -- but
// PostgreSQL then refuses to scan it into an int64 and fails the query, while
// this driver scans NULL as zero and says nothing. So on this side nothing
// observable changes if the COALESCE is deleted, which is why no test catches
// its removal; it stays because a tally that is a number by accident of the
// driver is a tally that breaks when the driver is upgraded.
const tallyColumns = `toInt64(count())                                AS records,
	       toInt64(sum(prompt_tokens))                     AS prompt_tokens,
	       toInt64(sum(completion_tokens))                 AS completion_tokens,
	       toInt64(coalesce(sum(cached_tokens), 0))        AS cached_tokens,
	       toInt64(sum(amount_micro))                      AS amount_micro`

// DetailTally totals the replica over a half-open window.
func (s *Store) DetailTally(ctx context.Context, from, to time.Time) (billing.Tally, error) {
	var t billing.Tally
	err := s.QueryRow(ctx, `
		SELECT `+tallyColumns+`
		FROM `+s.db+`.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2`, from, to).
		Scan(&t.Records, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens, &t.AmountMicro)
	if err != nil {
		return billing.Tally{}, fmt.Errorf("clickhouse: replica tally: %w", err)
	}
	return t, nil
}

// DetailGroups totals the replica per tenant and model.
//
// The key is built by billing.GroupKey rather than by concat() in this query,
// because the ledger's side of the comparison builds it too and two spellings of
// the same pair would report every group as disagreeing with itself.
func (s *Store) DetailGroups(ctx context.Context, from, to time.Time) ([]billing.GroupTally, error) {
	rows, err := s.Query(ctx, `
		SELECT tenant, model, `+tallyColumns+`
		FROM `+s.db+`.usage_detail
		WHERE occurred_at >= $1 AND occurred_at < $2
		GROUP BY tenant, model
		ORDER BY tenant, model`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []billing.GroupTally
	for rows.Next() {
		var tenant, model string
		var t billing.Tally
		if err := rows.Scan(&tenant, &model, &t.Records, &t.PromptTokens,
			&t.CompletionTokens, &t.CachedTokens, &t.AmountMicro); err != nil {
			return nil, fmt.Errorf("clickhouse: scanning a replica group: %w", err)
		}
		out = append(out, billing.GroupTally{Key: billing.GroupKey(tenant, model), Tally: t})
	}
	return out, rows.Err()
}
