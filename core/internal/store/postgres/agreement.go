package postgres

import (
	"context"
	"fmt"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

// The two questions an operator has about their own metering, and neither has
// an answer anywhere today.
//
// "Is the bill right?" needs the two measurements compared: the engine's own
// account of its output against the gateway's count of the text it forwarded,
// for the same endpoint, over the same window. They disagree a little every
// time — a prompt cache and a reasoning block are attributed differently by
// each side — so it takes a population, and it takes both populations for one
// key, before "the gateway counts answers 20% short" is a fact rather than a
// coincidence. A single endpoint drifting while the rest of the fleet is fine
// is invisible in any fleet-wide average, which is why the key is the endpoint.
//
// "How much of the invoicing is arithmetic?" needs the same rows broken down by
// what they were charged on. A fleet where every engine reports usage has an
// exact ledger; one where everything was counted has a ledger that is a percent
// or two out; one where rows were charged at the reservation has a ledger that
// is wrong by the ratio between max_tokens and the answer, and the two failure
// modes look identical in a total.

// SourceMix counts rows by what they were billed on, across a window.
//
// Read-only and unfiltered by tenant: it describes the fleet's metering, and a
// per-tenant version would answer a question nobody is asking.
func (db *DB) SourceMix(ctx context.Context, w Window) (map[string]int64, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	const q = `
		SELECT usage_source, COUNT(*)
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		 GROUP BY usage_source`
	rows, err := db.pool.Query(ctx, q, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("postgres: source mix: %w", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var (
			source string
			n      int64
		)
		if err := rows.Scan(&source, &n); err != nil {
			return nil, fmt.Errorf("postgres: scan source mix: %w", err)
		}
		out[source] = n
	}
	return out, rows.Err()
}

// Agreement returns the raw observations the billing package compares.
//
// It returns observations rather than a verdict on purpose: the thresholds are
// policy, and policy belongs in the domain package where it can be tested
// without a database and reasoned about in one place. This query is only the
// plumbing, and a query that also decided what counts as a fault would make
// changing the threshold a change to SQL.
func (db *DB) Agreement(ctx context.Context, w Window) ([]billing.Observation, error) {
	if err := w.valid(); err != nil {
		return nil, err
	}
	const q = `
		SELECT endpoint_id, completion_tokens, usage_known, usage_source, truncated
		  FROM usage_events
		 WHERE occurred_at >= $1 AND occurred_at < $2
		   AND usage_source <> 'reserved'
		   AND completion_tokens > 0`
	rows, err := db.pool.Query(ctx, q, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("postgres: agreement sample: %w", err)
	}
	defer rows.Close()

	var out []billing.Observation
	for rows.Next() {
		var o billing.Observation
		var source string
		if err := rows.Scan(&o.Key, &o.Output, &o.FromEngine, &source, &o.Truncated); err != nil {
			return nil, fmt.Errorf("postgres: scan agreement sample: %w", err)
		}
		o.Source = billing.Source(source)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate agreement samples: %w", err)
	}
	return out, nil
}
