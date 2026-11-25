package pipelinerun

import (
	"encoding/json"
	"testing"
)

// pipelinesRunDetail (design record 2026-11-24): a developer who does not own a
// run reads it under their own actor, then its steps and pipeline files under
// the OWNER's borrowed authority, and gets a projection -- never the rows.
func TestRunDetailProjectsAnotherOwnersRunForADeveloper(t *testing.T) {
	engine := newRecordingEngine()
	owner := "v1:identity:user:" + ownerID
	engine.answers[qPipelineRunView] = []any{map[string]any{
		"id": "v1:pipelines:run:r1", "ownerUserId": owner, "workRunId": "wr1", "status": "completed",
	}}
	engine.answers[qWorkStepsForOwnerRun] = []any{map[string]any{
		"id": "s1", "runId": "v1:work:run:wr1", "key": "tests.unit", "seq": 1, "status": "failed",
		"errorMessage": "exit 1", "logFileId": "f-log", "binding": map[string]any{"surface": "cluster"},
		"call":   map[string]any{"stage": "tests", "name": "unit", "packages": []any{"./x"}},
		"result": map[string]any{"status": "failed", "reason": "", "output": "not for the page"},
	}}
	engine.answers[qArtifactsForRun] = []any{
		map[string]any{"id": "a-log", "source": "pipeline", "sourceConceptRef": "v1:library:file:f-log", "title": "tests.unit.log", "format": "text", "archived": false, "ownerUserId": owner},
		map[string]any{"id": "a-note", "source": "uploaded", "sourceConceptRef": "v1:library:file:f-note", "title": "notes.txt"},
	}
	integ := New(Deps{Store: NewDSLStore(engine)})

	nodes, err := integ.handleRunDetail(personCtx(otherID), map[string]any{"runId": "r1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Steps, Files []map[string]any }
	if err := json.Unmarshal(nodes[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0]["key"] != "tests.unit" || got.Steps[0]["logFileId"] != "f-log" {
		t.Fatalf("steps = %v", got.Steps)
	}
	if call := got.Steps[0]["call"].(map[string]any); len(call) != 2 || call["stage"] != "tests" {
		t.Errorf("the step's call must be its stage and name only: %v", call)
	}
	if _, leaked := got.Steps[0]["result"].(map[string]any)["output"]; leaked {
		t.Error("the step's result leaked a field the page does not read")
	}
	if len(got.Files) != 1 || got.Files[0]["id"] != "a-log" {
		t.Fatalf("files = %v; want the pipeline file alone", got.Files)
	}
	if _, leaked := got.Files[0]["ownerUserId"]; leaked {
		t.Error("the file projection leaked a field the page does not read")
	}

	want := []struct{ construct, actor string }{
		{qPipelineRunForOwner, otherID}, {qPipelineRunView, otherID},
		{qWorkStepsForOwnerRun, owner}, {qArtifactsForRun, owner},
	}
	calls := engine.recorded()
	if len(calls) != len(want) {
		t.Fatalf("calls = %d (%v)", len(calls), calls)
	}
	for i, w := range want {
		c := calls[i]
		if constructOf(c.query) != w.construct || c.actor == nil || c.actor.UserId != w.actor || c.origin.IsInternal() {
			t.Errorf("call %d: %s as %+v (internal=%v); want %s as %s, unstamped", i, c.query, c.actor, c.origin.IsInternal(), w.construct, w.actor)
		}
	}
}

// A caller who can read the run by neither read is refused before anything is
// read under the owner.
func TestRunDetailRefusesARunTheCallerCannotRead(t *testing.T) {
	engine := newRecordingEngine()
	integ := New(Deps{Store: NewDSLStore(engine)})
	if _, err := integ.handleRunDetail(personCtx(otherID), map[string]any{"runId": "r1"}, 0); err == nil {
		t.Fatal("a run nobody let the caller read was answered")
	}
	if n := len(engine.recorded()); n != 2 {
		t.Fatalf("want the two caller reads and nothing under the owner, got %d calls", n)
	}
}
