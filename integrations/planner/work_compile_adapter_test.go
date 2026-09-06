package planner

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// recordingRunWriter stands in for the work integration. The adapter asserts
// on the FIELDS it hands over rather than on a rendered call, because the
// rendering is integrations/work's concern and is pinned there against the
// real parser.
type recordingRunWriter struct {
	runs   []string
	owners []string
	fields []map[string]any
	err    error
}

func (r *recordingRunWriter) RecordCompileOutcome(_ context.Context, ownerUserId, runId string, fields map[string]any) error {
	r.runs = append(r.runs, runId)
	r.owners = append(r.owners, ownerUserId)
	r.fields = append(r.fields, fields)
	return r.err
}

func adapterReq() workintegration.CompileRequest {
	return workintegration.CompileRequest{
		GoalId:      "v1:work:goal:g1",
		RunId:       "v1:work:run:r1",
		OwnerUserId: "u1",
		Statement:   "Summarise yesterday's support tickets",
		Input:       map[string]any{"day": "2026-09-04"},
	}
}

// A compile that FAILS must leave the run failed, not `compiling`. The
// wait-and-abandon sweep deliberately does not touch a run in `compiling`, so
// a run left there is one nothing will ever move -- and the symptom a person
// reports is "my goal never started", which names nothing.
func TestWorkCompiler_AFailedCompileFailsTheRun(t *testing.T) {
	w := &recordingRunWriter{}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: &countingCompileEngine{}}, w)
	req := adapterReq()
	req.Statement = "   " // CompileGoalForRun refuses an empty statement
	c.Compile(context.Background(), req)

	if len(w.fields) != 1 {
		t.Fatalf("expected exactly one write, got %d", len(w.fields))
	}
	f := w.fields[0]
	if f["status"] != "failed" {
		t.Fatalf("status = %v, want failed -- a run left in `compiling` is one the sweep will never touch", f["status"])
	}
	if f["errorCode"] != "compile_failed" || f["errorMessage"] == "" {
		t.Errorf("a failed compile must say why: %+v", f)
	}
	if f["finishedAt"] == nil {
		t.Error("a terminal run carries finishedAt")
	}
	if w.owners[0] != "u1" {
		t.Errorf("the write must borrow the goal owner's authority, got owner %q", w.owners[0])
	}
	if _, present := f["runId"]; present {
		t.Error("runId is the writer's parameter, not a field -- passing both invites them to disagree")
	}
}

// A successful compile records the template it chose. The run is opened
// BEFORE the template is known -- that ordering is the design -- and without
// this write the choice exists only in a log line.
func TestWorkCompiler_RecordsTheTemplateItChose(t *testing.T) {
	sig := work.GoalSignature(adapterReq().Statement, []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "v1:authoring:construct:c1", "name": "summariseTickets", "goalSignature": sig, "reliability": 0.9},
	}}
	w := &recordingRunWriter{}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: eng}, w)
	c.Compile(context.Background(), adapterReq())

	if len(w.fields) != 1 {
		t.Fatalf("expected one write, got %d", len(w.fields))
	}
	f := w.fields[0]
	if f["status"] != "running" {
		t.Errorf("status = %v, want running", f["status"])
	}
	if f["automationName"] != "summariseTickets" {
		t.Errorf("automationName = %v", f["automationName"])
	}
	if f["templateConstructId"] != "v1:authoring:construct:c1" {
		t.Errorf("templateConstructId = %v", f["templateConstructId"])
	}
	if f["variables"] == nil {
		t.Error("the bound args are recorded on the run, or a replay cannot know what it ran with")
	}
	if w.runs[0] != "v1:work:run:r1" {
		t.Errorf("wrote to run %q", w.runs[0])
	}
	// The whole point: this route reached no provider.
	if len(eng.aiCalls) != 0 {
		t.Fatalf("an exact catalog hit reached a provider: %v", eng.aiCalls)
	}
}

// A compiler that decides correctly and cannot record the decision is worse
// than none: the run moves to `running` in the log and never in the graph.
func TestNewWorkCompiler_RefusesWithoutALoopOrAWriter(t *testing.T) {
	if NewWorkCompiler(nil, &recordingRunWriter{}) != nil {
		t.Error("no loop means no compiler")
	}
	if NewWorkCompiler(&PlannerAgentLoop{}, nil) != nil {
		t.Error("no writer means no compiler -- a decision nothing records is not a decision")
	}
	var c *WorkCompiler
	c.Compile(context.Background(), adapterReq()) // must not panic
}
