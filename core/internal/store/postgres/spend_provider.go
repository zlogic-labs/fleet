package postgres

import (
	"context"
	"fmt"
)

// ProviderSpend is one commercial provider's charges inside a window.
type ProviderSpend struct {
	Provider string
	// Amount is real money, in the same micro units as Spend.UnitsMicro so the
	// two can be compared for sanity even though they must not be summed into a
	// single price.
	Amount int64
	// Requests is the number of ledger rows, which is what the vendor's own
	// statement can be checked against.
	Requests int64
}

// SpendByProvider totals what commercial providers charged inside a window.
//
// Separate from SpendByModel on purpose: a report that folded these into the
// model's token total would make a vendor route look like it used the fleet's
// GPUs, and the pool allocation would then charge a tenant for hardware that was
// somebody else's. The per-request count is returned so a difference from a
// vendor statement can be narrowed to volume rather than unit price.
//
// Providers with no rows are absent rather than zero: a table of zeros reads as
// "nothing was spent anywhere", which is the opposite of what an empty result
// means.
func (db *DB) SpendByProvider(ctx context.Context, w Window) ([]ProviderSpend, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	rows, err := db.pool.Query(ctx,
		`SELECT provider, COALESCE(SUM(amounts_micro), 0)::bigint, COUNT(*)
		   FROM usage_events
		  WHERE occurred_at >= $1 AND occurred_at < $2 AND provider <> ''
		  GROUP BY provider
		  ORDER BY provider`, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("postgres: spend by provider: %w", err)
	}
	defer rows.Close()

	out := []ProviderSpend{}
	for rows.Next() {
		var p ProviderSpend
		if err := rows.Scan(&p.Provider, &p.Amount, &p.Requests); err != nil {
			return nil, fmt.Errorf("postgres: scan provider spend: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
