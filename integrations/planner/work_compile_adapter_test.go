package planner

import (
	"context"
	"errors"
	"strings"
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
	// ceilings is what RunBudget answers, and budgetErr makes it fail --
	// which must leave the compile unbounded rather than refused.
	ceilings  work.Ceilings
	budgetErr error
	budgetFor []string
}

func (r *recordingRunWriter) RecordCompileOutcome(_ context.Context, ownerUserId, runId string, fields map[string]any) error {
	r.runs = append(r.runs, runId)
	r.owners = append(r.owners, ownerUserId)
	r.fields = append(r.fields, fields)
	return r.err
}

func (r *recordingRunWriter) RunBudget(_ context.Context, _, runId string) (work.Ceilings, error) {
	r.budgetFor = append(r.budgetFor, runId)
	return r.ceilings, r.budgetErr
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

// memql#5000. The authoring repair loop's cumulative LLM ceiling was read off
// a PLAN, and this path has none -- `emitAndRepairBundle` was handed an empty
// plan id and the gate returned "not exhausted" on its first line. So on the
// path the work spine actually uses, the loop was bounded by
// `repairAttemptCap` and nothing else, whatever ceiling the goal declared.
//
// The gate is asked for BEFORE the compile, so the failure below is that the
// adapter never asked.
func TestWorkCompiler_ReadsTheRunsCeilingsBeforeCompiling(t *testing.T) {
	w := &recordingRunWriter{ceilings: work.Ceilings{MaxModelCalls: 3}}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: &countingCompileEngine{}}, w)
	c.Compile(context.Background(), adapterReq())

	if len(w.budgetFor) == 0 {
		t.Fatal("the adapter compiled without reading the run's ceilings, so the authoring repair loop " +
			"is bounded by its attempt cap alone -- which is the defect")
	}
	if w.budgetFor[0] != adapterReq().RunId {
		t.Errorf("the ceilings were read for run %q, want %q", w.budgetFor[0], adapterReq().RunId)
	}
}

// A ceilings read that FAILS must leave the compile unbounded rather than
// refuse it: a transient database blip is not a reason to make every goal
// unrunnable, and the attempt cap still bounds the loop.
func TestWorkCompiler_ACeilingsReadFailureDoesNotRefuseTheCompile(t *testing.T) {
	sig := work.GoalSignature(adapterReq().Statement, []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "v1:authoring:construct:c1", "name": "summariseTickets", "goalSignature": sig, "reliability": 0.9},
	}}
	w := &recordingRunWriter{budgetErr: errors.New("database unreachable")}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: eng}, w)
	c.Compile(context.Background(), adapterReq())

	if len(w.fields) != 1 {
		t.Fatalf("expected one write, got %d", len(w.fields))
	}
	if got := w.fields[0]["status"]; got != "running" {
		t.Errorf("a ceilings read failure made the run %v; it must still compile", got)
	}
}

// The gate itself, at the boundary. A ceiling of zero is UNSET, never
// "nothing allowed" -- the reading that keeps a goal declaring no ceilings
// runnable rather than dead on arrival.
func TestCallCapGate(t *testing.T) {
	unset := callCapGate(0, "x")
	if blocked, _ := unset(context.Background(), 99); blocked {
		t.Error("a ceiling of zero blocked; zero is UNSET, not a refusal")
	}
	if blocked, _ := callCapGate(-1, "x")(context.Background(), 1); blocked {
		t.Error("a negative ceiling blocked")
	}

	gate := callCapGate(3, "this run's maxModelCalls ceiling")
	for _, made := range []int{0, 1, 2} {
		if blocked, _ := gate(context.Background(), made); blocked {
			t.Errorf("blocked at %d calls against a ceiling of 3", made)
		}
	}
	blocked, reason := gate(context.Background(), 3)
	if !blocked {
		t.Fatal("a job at its ceiling was allowed another call")
	}
	for _, want := range []string{"3", "maxModelCalls"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not name %q, so nobody can tell which ceiling stopped it: %q", want, reason)
		}
	}
}

// planBudgetGate keeps the plan path exactly as it was, including its
// deliberate fail-open on a plan-load error -- defensible where it is one of
// two ceilings, and what was NOT defensible as the only ceiling on a path
// with no plan at all.
func TestPlanBudgetGateIsUnboundedWithoutAPlan(t *testing.T) {
	l := &PlannerAgentLoop{engine: &countingCompileEngine{}}
	if blocked, _ := l.planBudgetGate("")(context.Background(), 999); blocked {
		t.Error("a gate with no plan blocked; there is no plan budget to be over")
	}
}
