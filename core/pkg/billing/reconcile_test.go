package billing

import (
	"encoding/json"
	"strings"
	"testing"
)

func tally(records, prompt, completion, cached, amount int64) Tally {
	return Tally{
		Records: records, PromptTokens: prompt, CompletionTokens: completion,
		CachedTokens: cached, AmountMicro: amount,
	}
}

func groupOf(key string, t Tally) GroupTally { return GroupTally{Key: key, Tally: t} }

func TestIdenticalStoresAgree(t *testing.T) {
	t1 := tally(3, 30, 40, 5, 1000)
	got := Reconcile(t1, t1,
		[]GroupTally{groupOf(GroupKey("acme", "qwen"), t1)},
		[]GroupTally{groupOf(GroupKey("acme", "qwen"), t1)}, 0)

	if !got.Agreed {
		t.Fatalf("two identical stores disagreed: fields=%v groups=%v", got.Fields, got.Groups)
	}
	if len(got.Fields) != 0 || len(got.Groups) != 0 || got.Omitted != 0 {
		t.Fatalf("an agreeing comparison carried findings: %+v", got)
	}
}

// The empty report has to marshal as empty lists, not nulls.
//
// The console renders this by asking the answer for .map and .length, so a null
// where a list belongs is not an empty report: it is a blank page, which is what
// the healthy case produced the first time this was opened against two stores
// that agreed. "No differences" is a measurement of zero, not an absence.
func TestAnAgreeingReportMarshalsEmptyListsRatherThanNulls(t *testing.T) {
	both := tally(3, 30, 40, 5, 1000)
	got := Reconcile(both, both,
		[]GroupTally{groupOf(GroupKey("acme", "qwen"), both)},
		[]GroupTally{groupOf(GroupKey("acme", "qwen"), both)}, 0)

	if !got.Agreed {
		t.Fatal("two identical stores were reported as disagreeing")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"fields":[]`, `"groups":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s is not in %s", want, raw)
		}
	}
}

func TestAReplicaShortOfRowsSaysHowMany(t *testing.T) {
	ledger := tally(10, 100, 200, 10, 5000)
	replica := tally(7, 70, 140, 7, 3500)
	got := Reconcile(ledger, replica, nil, nil, 0)

	if got.Agreed {
		t.Fatal("a replica with three rows missing was reported as agreeing")
	}
	if len(got.Fields) != 5 {
		t.Fatalf("every figure moved and %d were reported: %v", len(got.Fields), got.Fields)
	}
	if got.Fields[0] != (Difference{Field: "records", Ledger: 10, Replica: 7}) {
		t.Fatalf("records was not the first difference: %v", got.Fields[0])
	}
}

func TestExtraRowsAreReportedInTheOtherDirection(t *testing.T) {
	ledger := tally(7, 70, 140, 7, 3500)
	replica := tally(10, 100, 200, 10, 5000)
	got := Reconcile(ledger, replica, nil, nil, 0)

	if got.Agreed {
		t.Fatal("a replica with three rows too many was reported as agreeing")
	}
	if got.Fields[0].Ledger >= got.Fields[0].Replica {
		t.Fatalf("the direction was lost: %v", got.Fields[0])
	}
}

func TestGroupsThatCancelOutStillDisagree(t *testing.T) {
	ledger := tally(10, 100, 100, 0, 1000)
	replica := tally(10, 100, 100, 0, 1000)

	ledgerGroups := []GroupTally{
		groupOf(GroupKey("acme", "m"), tally(5, 50, 50, 0, 500)),
		groupOf(GroupKey("globex", "m"), tally(5, 50, 50, 0, 500)),
	}
	replicaGroups := []GroupTally{
		groupOf(GroupKey("acme", "m"), tally(4, 40, 40, 0, 400)),
		groupOf(GroupKey("globex", "m"), tally(6, 60, 60, 0, 600)),
	}

	got := Reconcile(ledger, replica, ledgerGroups, replicaGroups, 0)
	if len(got.Fields) != 0 {
		t.Fatalf("the totals should have matched: %v", got.Fields)
	}
	if got.Agreed {
		t.Fatal("one tenant short and another long was reported as agreement")
	}
	if len(got.Groups) != 2 {
		t.Fatalf("want both groups named, got %d", len(got.Groups))
	}
	if got.Groups[0].Reason != "1 row more in the ledger than in the replica" {
		t.Fatalf("acme's reason reads wrong: %q", got.Groups[0].Reason)
	}
	if got.Groups[1].Reason != "1 row more in the replica than in the ledger" {
		t.Fatalf("globex's reason reads wrong: %q", got.Groups[1].Reason)
	}
}

func TestAGroupPresentOnOneSideOnlyIsNamed(t *testing.T) {
	t1 := tally(2, 20, 20, 0, 200)
	got := Reconcile(t1, tally(0, 0, 0, 0, 0),
		[]GroupTally{groupOf(GroupKey("acme", "m"), t1)}, nil, 0)

	if len(got.Groups) != 1 {
		t.Fatalf("want the absent group named, got %d", len(got.Groups))
	}
	g := got.Groups[0]
	if g.Ledger.Records != 2 || g.Replica.Records != 0 {
		t.Fatalf("the group's two sides are wrong: %+v", g)
	}
}

func TestTheSameCountWithDifferentFiguresIsItsOwnReason(t *testing.T) {
	got := Reconcile(tally(4, 40, 40, 0, 400), tally(4, 40, 41, 0, 400),
		[]GroupTally{groupOf(GroupKey("acme", "m"), tally(4, 40, 40, 0, 400))},
		[]GroupTally{groupOf(GroupKey("acme", "m"), tally(4, 40, 41, 0, 400))}, 0)

	if got.Groups[0].Reason != "the same number of rows, disagreeing on what is in them" {
		t.Fatalf("reason: %q", got.Groups[0].Reason)
	}
	if got.Groups[0].Fields[0].Field != "completionTokens" {
		t.Fatalf("the field that moved was not named: %v", got.Groups[0].Fields)
	}
}

func TestTheGroupListIsCappedAndCountsWhatItLeftOut(t *testing.T) {
	var ledgerGroups, replicaGroups []GroupTally
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		ledgerGroups = append(ledgerGroups, groupOf(GroupKey(name, "m"), tally(2, 20, 20, 0, 200)))
		replicaGroups = append(replicaGroups, groupOf(GroupKey(name, "m"), tally(1, 10, 10, 0, 100)))
	}
	got := Reconcile(tally(10, 100, 100, 0, 1000), tally(5, 50, 50, 0, 500),
		ledgerGroups, replicaGroups, 2)

	if len(got.Groups) != 2 {
		t.Fatalf("want two groups, got %d", len(got.Groups))
	}
	if got.Omitted != 3 {
		t.Fatalf("want three counted as left out, got %d", got.Omitted)
	}
	if got.Agreed {
		t.Fatal("a capped report is not agreement")
	}
}

func TestGroupsAreNamedInAFixedOrder(t *testing.T) {
	var ledgerGroups []GroupTally
	for _, name := range []string{"globex", "acme", "initech"} {
		ledgerGroups = append(ledgerGroups, groupOf(GroupKey(name, "m"), tally(2, 20, 20, 0, 200)))
	}
	got := Reconcile(tally(6, 60, 60, 0, 600), tally(0, 0, 0, 0, 0), ledgerGroups, nil, 0)

	want := []string{"acme/m", "globex/m", "initech/m"}
	for i, k := range want {
		if got.Groups[i].Key != k {
			t.Fatalf("group %d is %q, want %q", i, got.Groups[i].Key, k)
		}
	}
}

func TestANonPositiveLimitFallsBackToTheDefault(t *testing.T) {
	got := Reconcile(tally(1, 1, 1, 0, 1), tally(0, 0, 0, 0, 0), nil, nil, 0)
	if !got.Agreed && got.Fields == nil {
		t.Fatal("the comparison produced nothing at all")
	}
	// The limit only bounds the group list, so a comparison with no groups is
	// unaffected by it; what matters is that a zero limit is not read as
	// "report no groups".
	var ledgerGroups, replicaGroups []GroupTally
	ledgerGroups = append(ledgerGroups, groupOf("x/m", tally(1, 1, 1, 0, 1)))
	got = Reconcile(tally(1, 1, 1, 0, 1), tally(0, 0, 0, 0, 0), ledgerGroups, replicaGroups, 0)
	if len(got.Groups) != 1 || got.Omitted != 0 {
		t.Fatalf("a zero limit dropped the group: %+v", got)
	}
}
