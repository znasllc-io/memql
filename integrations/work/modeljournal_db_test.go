package work

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// TestModelJournalDB_TheDecisionAndTheRouterCallLandOnTheRow runs the write
// for real (synthesis2 fix 8). The writer rendering the two fields proves
// nothing about the row: a mutation that does not declare an argument, or an
// object the concept's type check refuses, loses the WHOLE row -- and the
// journal logs one WARN and carries on, which is exactly how a journal goes
// silently empty. So the row is written through the real engine and read back
// through the real query.
func TestModelJournalDB_TheDecisionAndTheRouterCallLandOnTheRow(t *testing.T) {
	eng := openWorkTestEngine(t)
	i := New(eng, testLogger())
	owner := "dbtest-work-journal-" + time.Now().UTC().Format("20060102150405.000000000")
	runId := newRowId(runConcept)
	j := &ModelJournal{store: i.store()}

	if err := j.Record(actorCtx(owner), owner, memqlengine.JournaledCall{
		RunId: runId, StepKey: "triage", RequestHash: "hash-" + owner,
		Provider: "fleet:qwen3.5:4b", Model: "qwen3.5:4b", Served: "local",
		Response:     map[string]any{"text": `{"complexity":"trivial"}`},
		RouterCallId: "v1:router:call:c-" + owner,
		Decision:     servedDecision(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := readModelCalls(actorCtx(owner), i.store(), owner, runId)
	if err != nil {
		t.Fatalf("reading the run's journal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the run's journal has %d rows, want the one just written", len(rows))
	}
	row := rows[0]
	if got := rowString(row, "routerCallId"); got != "v1:router:call:c-"+owner {
		t.Errorf("routerCallId=%q did not land on the row", got)
	}
	if got := rowString(row, "provider"); got != "fleet:qwen3.5:4b" {
		t.Errorf("provider=%q", got)
	}
	decision := rowMap(row, "decision")
	if decision == nil {
		t.Fatalf("decision did not land on the row: %v", row)
	}
	if decision["door"] != "local" || decision["rule"] != "fastLocalFirst" {
		t.Errorf("decision=%v", decision)
	}
	considered, _ := decision["considered"].([]any)
	if len(considered) != 2 {
		t.Fatalf("decision.considered=%#v, want both lines of the route", decision["considered"])
	}
	app, _ := considered[0].(map[string]any)
	if app["entry"] != "app:claude-code" || app["door"] != "app" {
		t.Errorf("considered[0]=%v, want the app source passed over", app)
	}
}

// A @usesDeployerFleet call acts for a person on a run that stays the deployment's:
// its row is stamped to the person, and the blank-owner run's own reader must still
// find it, or a strict replay of the workflow would report a divergence blaming the
// prompt (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md).
func TestModelJournalDB_ABorrowedCallOnADeploymentsRunIsFound(t *testing.T) {
	eng := openWorkTestEngine(t)
	i := New(eng, testLogger())
	deployer := "dbtest-work-journal-deployer-" + time.Now().UTC().Format("20060102150405.000000000")
	runId := newRowId(runConcept)
	j := &ModelJournal{store: i.store()}

	if err := j.Record(actorCtx(deployer), "", memqlengine.JournaledCall{
		RunId: runId, StepKey: "generated", RequestHash: "hash-" + deployer,
		Provider: "fleet:qwen3.8:27b", Model: "qwen3.8:27b", Served: "local",
		Response: map[string]any{"text": `{"brief":"ok"}`},
		Decision: servedDecision(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	cluster := auth.ContextWithSystemActor(context.Background(), "deployer-fleet-journal-test")
	rows, err := readModelCalls(cluster, i.store(), "", runId)
	if err != nil {
		t.Fatalf("reading the run's journal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the deployment's run reads %d model calls, want the borrowed one", len(rows))
	}
	if got := rowString(rows[0], "provider"); got != "fleet:qwen3.8:27b" {
		t.Errorf("provider=%q", got)
	}
}
