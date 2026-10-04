package work

import (
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// servedDecision is a planner node's triage as a decision: the app source it
// cannot open passed over, the local model that served.
func servedDecision() *airoute.Decision {
	return &airoute.Decision{
		Level: airoute.LevelFast, RequestedLevel: airoute.LevelFast, ServedLevel: airoute.LevelFast,
		Rule: "fastLocalFirst", Policy: "fastLocalFirst", Door: airoute.DoorLocal, Outcome: airoute.OutcomeOK,
		MinContextTokens: 8192,
		Considered: []airoute.ConsideredEntry{
			{Entry: "app:claude-code", Door: airoute.DoorApp, Reason: "app sources run on the agent holding the machine; this planner node cannot open one"},
			{Entry: "fleet:qwen3.5:4b", Door: airoute.DoorLocal, Reason: "selected"},
		},
	}
}

// THE JOURNAL ROW CARRIES THE DECISION RECORD BEHIND THE CALL (synthesis2 fix
// 8): the v1:router:call row that records it, and the decision itself -- so a
// run's journal says not only which source answered but why that one, and
// which sources the route passed over on the way.
func TestModelJournalRecordsTheRouterCallAndTheDecision(t *testing.T) {
	i, eng := newTestIntegration(t)
	j := &ModelJournal{store: i.store()}
	if err := j.Record(callerContext("u1"), "u1", memqlengine.JournaledCall{
		RunId: "v1:work:run:r1", StepKey: "triage", RequestHash: "abc",
		Provider: "fleet:qwen3.5:4b", Model: "qwen3.5:4b", Served: "local",
		Response:     map[string]any{"text": `{"complexity":"trivial"}`},
		RouterCallId: "v1:router:call:c1",
		Decision:     servedDecision(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	args := eng.callTo(t, "createWorkModelCall").Args(t)
	if args["routerCallId"] != "v1:router:call:c1" {
		t.Errorf("routerCallId=%v, want the decision record behind the call", args["routerCallId"])
	}
	decision, ok := args["decision"].(map[string]any)
	if !ok {
		t.Fatalf("decision=%#v, want an object", args["decision"])
	}
	for key, want := range map[string]any{
		"level": "fast", "servedLevel": "fast", "rule": "fastLocalFirst", "policy": "fastLocalFirst",
		"door": "local", "outcome": "ok", "degraded": false,
	} {
		if decision[key] != want {
			t.Errorf("decision.%s=%v, want %v", key, decision[key], want)
		}
	}
	considered, _ := decision["considered"].([]any)
	if len(considered) != 2 {
		t.Fatalf("decision.considered=%#v, want the two lines of the route", decision["considered"])
	}
	app, _ := considered[0].(map[string]any)
	if app["entry"] != "app:claude-code" || app["door"] != "app" || app["reason"] == "" {
		t.Errorf("considered[0]=%v, want the app source passed over with its reason", app)
	}
}

// A ROW WITH NO DECISION SENDS NONE. A journal-served row asked no source, and
// an optional object given null fails the concept's type check -- which would
// lose the whole row.
func TestModelJournalOmitsAnAbsentDecision(t *testing.T) {
	i, eng := newTestIntegration(t)
	j := &ModelJournal{store: i.store()}
	if err := j.Record(callerContext("u1"), "u1", memqlengine.JournaledCall{
		RunId: "v1:work:run:r1", RequestHash: "abc", Provider: "chat54Mini", Model: "gpt-5.4-mini", Served: "journal",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	args := eng.callTo(t, "createWorkModelCall").Args(t)
	if _, present := args["decision"]; present {
		t.Errorf("a row with no decision sent decision=%#v", args["decision"])
	}
}
