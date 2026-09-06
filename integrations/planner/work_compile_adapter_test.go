package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

func adapterReq() workintegration.CompileRequest {
	return workintegration.CompileRequest{
		GoalId:      "v1:work:goal:g1",
		RunId:       "v1:work:run:r1",
		OwnerUserId: "u1",
		Statement:   "Summarise yesterday's support tickets",
		Input:       map[string]any{"day": "2026-09-04"},
	}
}

func updateCallsIn(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "updateWorkRun(") {
			out = append(out, c)
		}
	}
	return out
}

// A compile that FAILS must leave the run failed, not `compiling`. The
// wait-and-abandon sweep deliberately does not touch a run in `compiling`,
// so a run left there is one nothing will ever move -- and the symptom a
// person reports is "my goal never started", which names nothing.
func TestWorkCompiler_AFailedCompileFailsTheRun(t *testing.T) {
	eng := &countingCompileEngine{}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: eng})
	req := adapterReq()
	req.Statement = "   " // CompileGoalForRun refuses an empty statement
	c.Compile(context.Background(), req)

	updates := updateCallsIn(eng.queries)
	if len(updates) != 1 {
		t.Fatalf("expected exactly one updateWorkRun, got %v", eng.queries)
	}
	args := healerArgs(t, updates[0])
	if args["status"] != "failed" {
		t.Fatalf("status = %v, want failed -- a run left in `compiling` is one the sweep will never touch", args["status"])
	}
	if args["errorCode"] != "compile_failed" || args["errorMessage"] == "" {
		t.Errorf("a failed compile must say why: %+v", args)
	}
	if args["finishedAt"] == nil {
		t.Error("a terminal run carries finishedAt")
	}
}

// A successful compile records the template it chose. The run is opened
// BEFORE the template is known -- that ordering is the design, so the model
// calls compilation makes have a home from the first one -- and without this
// write the choice exists only in a log line.
func TestWorkCompiler_RecordsTheTemplateItChose(t *testing.T) {
	// Computed the same way compile does, so fixture and code cannot drift.
	sig := work.GoalSignature(adapterReq().Statement, []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "v1:authoring:construct:c1", "name": "summariseTickets", "goalSignature": sig, "reliability": 0.9},
	}}
	c := NewWorkCompiler(&PlannerAgentLoop{engine: eng})
	c.Compile(context.Background(), adapterReq())

	updates := updateCallsIn(eng.queries)
	if len(updates) != 1 {
		t.Fatalf("expected one updateWorkRun, got %v", eng.queries)
	}
	args := healerArgs(t, updates[0])
	if args["status"] != "running" {
		t.Errorf("status = %v, want running", args["status"])
	}
	if args["automationName"] != "summariseTickets" {
		t.Errorf("automationName = %v", args["automationName"])
	}
	if args["templateConstructId"] != "v1:authoring:construct:c1" {
		t.Errorf("templateConstructId = %v", args["templateConstructId"])
	}
	if args["variables"] == nil {
		t.Error("the bound args are recorded on the run, or a replay cannot know what it ran with")
	}
	// The whole point: this route reached no provider.
	if len(eng.aiCalls) != 0 {
		t.Fatalf("an exact catalog hit reached a provider: %v", eng.aiCalls)
	}
}

func TestNewWorkCompiler_NilLoopYieldsNoCompiler(t *testing.T) {
	if NewWorkCompiler(nil) != nil {
		t.Fatal("no loop means no compiler, so app wiring can call SetCompiler unconditionally")
	}
	var c *WorkCompiler
	c.Compile(context.Background(), adapterReq()) // must not panic
}
