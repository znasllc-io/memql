package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// pipelinesRunDetail over REAL rows (design record 2026-11-24): a run owned by
// one person, a work step and two Library artifacts of its work run, one of
// them a pipeline file. A developer answers with the step and that file alone;
// a writer and an admin are refused at the developer floor of the run read.
func TestRunDetailOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	suffix := id.NewShortId()
	p := testPipeline(DeliveryWebhook)
	p.ID, p.OwnerUserID = "detail-p-"+suffix, "detail-owner-"+suffix
	run := queuedRun(p, shaA)
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	integ := New(Deps{Store: store, Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
		return eng.Execute(ctx, q)
	}), nil, "detail-agent-"+suffix)})
	work, err := integ.snapshot().Journal.Begin(ctx, workjournal.Work{
		OwnerUserID: p.OwnerUserID, Template: "detail-proof", GoalKey: run.ID, RunKey: "1",
		Statement: "Prove the detail read", QueueSteps: true, TriggeredBy: "pipeline:affected",
		Steps: []workjournal.StepDecl{{Key: "build", StepType: "exec"}},
	})
	if err != nil || work == nil {
		t.Fatalf("Begin: %v", err)
	}
	wr := work.RunID()
	if err := store.UpdateRun(ctx, p.OwnerUserID, run.ID, RunPatch{WorkRunID: &wr}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	for _, a := range []struct{ ref, source string }{{"detail-log-" + suffix, "pipeline"}, {"detail-note-" + suffix, "user_created"}} {
		q := fmt.Sprintf(`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "document", source: %s, title: %s, format: "text", producedByRunId: %s)`,
			langparser.QuoteString(a.ref), langparser.QuoteString(p.OwnerUserID), langparser.QuoteString(a.source), langparser.QuoteString(a.ref), langparser.QuoteString(bareID(wr)))
		if _, err := eng.Execute(signedIn(p.OwnerUserID), q); err != nil {
			t.Fatalf("createArtifact %s: %v", a.source, err)
		}
	}

	as := func(role auth.Role) context.Context { return signedInAs("detail-viewer-"+suffix+"-"+string(role), role) }
	nodes, err := integ.handleRunDetail(as(auth.RoleDeveloper), map[string]any{"runId": run.ID}, 0)
	if err != nil {
		t.Fatalf("a developer is refused: %v", err)
	}
	var got struct{ Steps, Files []map[string]any }
	if err := json.Unmarshal(nodes[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0]["key"] != "build" {
		t.Errorf("steps = %v; want the one work step", got.Steps)
	}
	if len(got.Files) != 1 || got.Files[0]["title"] != "detail-log-"+suffix {
		t.Errorf("files = %v; want the pipeline file alone", got.Files)
	}
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleAdmin} {
		if _, err := integ.handleRunDetail(as(role), map[string]any{"runId": run.ID}, 0); err == nil {
			t.Errorf("a %s read another person's run detail", role)
		}
	}
}
