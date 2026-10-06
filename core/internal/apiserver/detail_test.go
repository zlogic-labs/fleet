package apiserver

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// No replica configured is a supported deployment, not a fault, and the report
// has to say which of the two it is: an operator reading "unavailable" on a
// fleet that never configured one has nothing to fix, and an operator reading it
// on a fleet that did has a URL to check.
func TestNoReplicaConfiguredSaysSoRatherThanFailing(t *testing.T) {
	d := &detailConn{cfg: clickhouse.Config{}, log: discardLog()}

	store, reason := d.get()
	if store != nil {
		t.Fatal("a store was returned with no URL configured")
	}
	if !strings.Contains(reason, "FLEET_CLICKHOUSE_URL") {
		t.Fatalf("the reason does not name the variable to set: %q", reason)
	}
}

// An unreachable replica is reported once and then answered from memory, so the
// console's poll does not spend the dial timeout on every request while the
// database is down -- which would make the page slower than the outage.
func TestAnUnreachableReplicaIsNotRedialledOnEveryRequest(t *testing.T) {
	d := &detailConn{
		// A port nothing is listening on. 127.0.0.1 rather than a name: this
		// test must not depend on a resolver, and a refused connection is the
		// fastest way to reach the failure being tested.
		cfg: clickhouse.Config{URL: "http://127.0.0.1:1", Database: "fleet_detail"},
		log: discardLog(),
	}

	_, first := d.get()
	if first == "" {
		t.Fatal("an unreachable store reported no reason")
	}
	if !strings.Contains(first, "could not be reached") {
		t.Fatalf("the reason does not say what went wrong: %q", first)
	}
	if d.nextTry.IsZero() {
		t.Fatal("no retry was scheduled, so every request will dial again")
	}

	_, second := d.get()
	if second != first {
		t.Fatalf("the second answer differs from the first: %q then %q", first, second)
	}
}

// Closing a connection that was never opened must not panic, because Close runs
// on every shutdown including the ones where nothing was ever configured.
func TestClosingAnUnopenedConnectionIsHarmless(t *testing.T) {
	d := &detailConn{cfg: clickhouse.Config{}, log: discardLog()}
	d.Close()
	d.Close()
}
