package automations

import (
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// The agent that runs a run's steps reads the owner's routing choice off the
// run ROW -- it holds nothing of the node that took the turn -- so the journal
// is where the choice crosses the hop.
func TestTheRunJournalCarriesTheOwnersRouteChoice(t *testing.T) {
	run := map[string]any{
		"id": "v1:work:run:r1", "goalId": "v1:work:goal:g1", "ownerUserId": "v1:identity:user:alice",
		"routing": map[string]any{"source": "app:claude-code", "level": "strong", "by": "v1:identity:user:alice"},
	}
	j, err := runJournalFromRows(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := common.RouteChoice{Source: "app:claude-code", Level: "strong", By: "v1:identity:user:alice"}
	if j.Routing != want {
		t.Fatalf("journal routing = %+v, want %+v", j.Routing, want)
	}

	// A run nobody chose for -- and every run written before the field
	// existed -- routes by the rules.
	j, err = runJournalFromRows(map[string]any{"id": "v1:work:run:r2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Routing.IsZero() {
		t.Fatalf("a run with no routing read as %+v", j.Routing)
	}
}
