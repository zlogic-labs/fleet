package billing

import (
	"fmt"
	"sort"
)

// Two stores holding the same records.
//
// usage_events is the ledger. It is written inside the request's settlement, it
// is what a closed period was computed from, and it is the only copy that cannot
// be regenerated. usage_detail in ClickHouse is a replica built from it, written
// asynchronously through a queue that drops on overflow, and read when a
// question is the wrong shape for a relational table.
//
// A replica that has drifted is not a theoretical worry. The queue drops under
// load, its writes are not retried, the store can be restored from an older
// backup, and a row the queue already wrote can be written a second time by the
// backfill catching up behind it. Each of those leaves two stores that both look
// healthy and disagree, and nothing in the product would say so: a report reads
// from one of them and therefore agrees with itself by construction.
//
// So this compares them, and only compares them. Nothing here writes, and
// nothing here can. The ledger is authoritative, so a difference is a fact about
// the replica rather than a correction to apply to the books — which is also why
// a closed period's figures never move because of what this finds (§11.10).

// Tally is what one store says about a set of usage records.
//
// Records is a count and the rest are sums, so a comparison can tell "a row is
// missing" apart from "a row is wrong". The first moves the count and the second
// does not, and the two have different causes and different remedies.
type Tally struct {
	Records          int64 `json:"records"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
	AmountMicro      int64 `json:"amountMicro"`
}

// GroupKey is how a (tenant, model) pair is spelled on both sides of the
// comparison.
//
// One function rather than a format string inside each store's SQL: the two
// queries are written in different languages against different servers, and a
// separator that differed between them would report every group as disagreeing
// with itself.
func GroupKey(tenant, model string) string { return tenant + "/" + model }

// GroupTally is a tally with the key it was grouped under.
type GroupTally struct {
	Key string
	Tally
}

// Difference is one figure the two stores disagree about.
type Difference struct {
	Field   string `json:"field"`
	Ledger  int64  `json:"ledger"`
	Replica int64  `json:"replica"`
}

// GroupDiff is one key's disagreement, with the arithmetic that produced it.
type GroupDiff struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	// Ledger and Replica are the whole tallies, not only the fields that
	// differ: a row that is missing moves every sum it contributed to, and
	// showing the count alone would leave the reader to work out which of the
	// figures moved because of it.
	Ledger  Tally        `json:"ledger"`
	Replica Tally        `json:"replica"`
	Fields  []Difference `json:"fields"`
}

// Reconciliation is the answer to "do the reports agree with the books".
type Reconciliation struct {
	Ledger  Tally `json:"ledger"`
	Replica Tally `json:"replica"`
	// Agreed is false when anything differs, including a group whose rows
	// cancel out at the top level. One tenant missing and another duplicated is
	// not agreement, and a total is exactly the figure that cannot see it.
	Agreed  bool         `json:"agreed"`
	Fields  []Difference `json:"fields"`
	Groups  []GroupDiff  `json:"groups"`
	Omitted int          `json:"omitted"`
}

// DefaultGroupLimit bounds how many disagreeing groups are named.
//
// Fifty is enough that a fleet with one bad endpoint, one restored backup or one
// duplicated batch sees all of it, and small enough that a replica restored from
// nothing does not return a table of every model in the ledger. What is left out
// is counted rather than dropped.
const DefaultGroupLimit = 50

// Reconcile compares the two stores.
//
// The totals and the groups are both taken as given rather than one being
// derived from the other, because they are not the same measurement: a grouped
// query cannot report rows whose grouping key is empty, and rows with no tenant
// are exactly the ones a query that joins them to something would lose.
func Reconcile(ledger, replica Tally, ledgerGroups, replicaGroups []GroupTally, limit int) Reconciliation {
	if limit <= 0 {
		limit = DefaultGroupLimit
	}
	out := Reconciliation{Ledger: ledger, Replica: replica}
	out.Fields = diffFields(ledger, replica)

	diffs := groupDiffs(ledgerGroups, replicaGroups)
	found := len(diffs)
	if found > limit {
		out.Omitted = found - limit
		diffs = diffs[:limit]
	}
	out.Groups = diffs
	out.Agreed = len(out.Fields) == 0 && found == 0

	// Both lists are emitted as lists even when they are empty, because they are
	// read as lists: a nil slice marshals to JSON null, and the console that
	// renders this asks the answer for `.map` and `.length`. A null there is not
	// an empty report, it is a blank page -- which is exactly what the healthy
	// case produced the first time this was opened against two stores that
	// agreed. "No differences" is a measurement of zero, not an absence, and the
	// wire shape has to say so.
	if out.Fields == nil {
		out.Fields = []Difference{}
	}
	if out.Groups == nil {
		out.Groups = []GroupDiff{}
	}
	return out
}

func groupDiffs(ledgerGroups, replicaGroups []GroupTally) []GroupDiff {
	ledger := index(ledgerGroups)
	replica := index(replicaGroups)

	keys := make(map[string]struct{}, len(ledger)+len(replica))
	for k := range ledger {
		keys[k] = struct{}{}
	}
	for k := range replica {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []GroupDiff
	for _, k := range sorted {
		lt, rt := ledger[k], replica[k]
		fields := diffFields(lt, rt)
		if len(fields) == 0 {
			continue
		}
		out = append(out, GroupDiff{
			Key: k, Reason: reasonFor(lt, rt), Ledger: lt, Replica: rt, Fields: fields,
		})
	}
	return out
}

func index(groups []GroupTally) map[string]Tally {
	out := make(map[string]Tally, len(groups))
	for _, g := range groups {
		out[g.Key] = g.Tally
	}
	return out
}

// reasonFor describes the shape of the difference without diagnosing it.
//
// The count decides the sentence because the count is what distinguishes the two
// failure modes that matter: a replica short of rows is a replica that lost
// writes, and a replica with extra rows is one that wrote some twice. A group
// whose count matches is a different problem again — the row is there and the
// figures in it are not what the ledger recorded.
func reasonFor(l, r Tally) string {
	switch {
	case r.Records < l.Records:
		return fmt.Sprintf("%s more in the ledger than in the replica", rowCount(l.Records-r.Records))
	case r.Records > l.Records:
		return fmt.Sprintf("%s more in the replica than in the ledger", rowCount(r.Records-l.Records))
	default:
		return "the same number of rows, disagreeing on what is in them"
	}
}

// rowCount is a row count in the number a reader would use.
//
// A single missing row is the common case — one dropped batch, one request that
// settled either side of midnight — and "1 more rows" is the sentence that
// appears in exactly the report somebody is reading closely.
func rowCount(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// diffFields lists the figures that differ, in a fixed order.
//
// Ordered rather than map-driven so two runs over the same rows produce the same
// report: an operator comparing this month's output with last month's should be
// comparing the numbers, not the iteration order of a Go map.
func diffFields(l, r Tally) []Difference {
	var out []Difference
	for _, f := range [...]struct {
		name            string
		ledger, replica int64
	}{
		{"records", l.Records, r.Records},
		{"promptTokens", l.PromptTokens, r.PromptTokens},
		{"completionTokens", l.CompletionTokens, r.CompletionTokens},
		{"cachedTokens", l.CachedTokens, r.CachedTokens},
		{"amountMicro", l.AmountMicro, r.AmountMicro},
	} {
		if f.ledger != f.replica {
			out = append(out, Difference{Field: f.name, Ledger: f.ledger, Replica: f.replica})
		}
	}
	return out
}
