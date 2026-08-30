package planner

import (
	"strings"
	"testing"
)

// TestUserGoalIsExecutable is the memql#4688 regression, and it is one boolean.
//
// The bug it guards is not a wrong answer -- it is NO answer: a userGoal plan
// reached running and every subscriber declined it, so it sat in running
// forever having produced nothing, with no error raised anywhere. A plan that
// silently does nothing is indistinguishable, from the outside, from a plan
// that is still working.
func TestUserGoalIsExecutable(t *testing.T) {
	if !executableViaApprovedPath(userGoalPlanKind) {
		t.Fatal("userGoal is not executable via the approved path.\n\n" +
			"Nothing else consumes it: runPhasedFanOut is referenced only by its own tests, and " +
			"no subscriber reads v1:planner:task rows. Excluding it here means Run does nothing, " +
			"forever, with no error (memql#4688).")
	}
	// The exclusions are load-bearing -- these kinds have their own dispatchers
	// and a second one would double-execute them.
	for _, kind := range []string{"trainSpecialist", "embedDomainItems", "adHocAction", "agentInvocation", "analyzeFile"} {
		if executableViaApprovedPath(kind) {
			t.Errorf("%q must NOT flow through the approved path; it has its own dispatcher", kind)
		}
	}
}

// TestUserGoalTurnCarriesTheDirectiveAsATrustedHint pins the memql#1102 shape.
// The directive is the system's own instruction, so it must be a hint. Folded
// into the history it lands inside the untrusted-conversation block, where the
// agent prompt's injection guard is entitled to refuse it -- which is exactly
// how the produce flow once ended a turn by declining to do the work.
func TestUserGoalTurnCarriesTheDirectiveAsATrustedHint(t *testing.T) {
	plan := planExecutionRow{Kind: userGoalPlanKind, Goal: "write up the Q3 numbers"}
	history, hints := buildExecutionTurn("v1:planner:plan:p1", plan, nil)

	if hints["execution_directive"] == "" {
		t.Fatal("a userGoal dispatch carries no execution directive")
	}
	if len(history) != 1 || history[0].Content != "write up the Q3 numbers" {
		t.Fatalf("history must be the user's goal and nothing else, got %d message(s): %+v", len(history), history)
	}
	for _, m := range history {
		if strings.Contains(m.Content, "EXECUTE THIS GOAL NOW") {
			t.Error("the directive leaked into the untrusted history block (memql#1102)")
		}
	}
	if hints["trigger"] != "plan_approved" || hints["plan_id"] != "v1:planner:plan:p1" {
		t.Errorf("the standard approved-path hints must survive: %+v", hints)
	}
}

// TestOtherKindsGetNoUserGoalDirective: the directive says "you are the
// executor, do not delegate". produceArtifact has its own, differently-worded
// one, and shipping both would give the agent two overlapping instructions.
func TestOtherKindsGetNoUserGoalDirective(t *testing.T) {
	for _, kind := range []string{produceArtifactPlanKind, "scopeElevation"} {
		_, hints := buildExecutionTurn("p", planExecutionRow{Kind: kind, Goal: "g"}, nil)
		if hints["execution_directive"] != "" {
			t.Errorf("%s picked up the userGoal directive", kind)
		}
		if hints["plan_steps"] != "" {
			t.Errorf("%s picked up plan_steps; only userGoal carries a task list", kind)
		}
	}
}

// TestPlanStepsRideTheTurn: the user reviewed this list before clicking Run.
// Discarding it would make Run mean something other than what the screen showed.
func TestPlanStepsRideTheTurn(t *testing.T) {
	tasks := []planTaskSummary{
		{Order: 1, Title: "Pull the ledger", Phase: "gather", Status: "succeeded"},
		{Order: 2, Title: "Summarize by region", Phase: "analyze", Status: "pending", Description: "one paragraph each"},
	}
	_, hints := buildExecutionTurn("p", planExecutionRow{Kind: userGoalPlanKind, Goal: "g"}, tasks)

	steps := hints["plan_steps"]
	if !strings.Contains(steps, "Pull the ledger") || !strings.Contains(steps, "Summarize by region") {
		t.Fatalf("plan_steps lost a step: %q", steps)
	}
	// A finished step must be MARKED, not dropped: a resumed plan whose agent
	// is shown only what remains cannot tell whether the step it needs the
	// output of has already run.
	if !strings.Contains(steps, "ALREADY SUCCEEDED") {
		t.Errorf("a completed step must be marked done, got: %q", steps)
	}
	if !strings.Contains(steps, "one paragraph each") {
		t.Errorf("step description dropped: %q", steps)
	}
}

// TestTaskSummaryOrderingIsStableAndDeterministic. The steps become prompt
// text; a list that reorders between two runs is a different prompt, which
// defeats the identical-request circuit breaker and makes one repeated failure
// look like several different ones.
func TestTaskSummaryOrderingIsStableAndDeterministic(t *testing.T) {
	rows := []map[string]any{
		{"id": "t3", "payload": map[string]any{"title": "third", "sequenceNumber": float64(3), "category": "semantic"}},
		{"id": "t1", "payload": map[string]any{"title": "first", "sequenceNumber": float64(1), "category": "semantic"}},
		{"id": "t2", "payload": map[string]any{"title": "second", "sequenceNumber": float64(2), "category": "semantic"}},
	}
	want := []string{"first", "second", "third"}
	for run := 0; run < 5; run++ {
		got := taskSummariesFromRows(rows)
		if len(got) != 3 {
			t.Fatalf("run %d: got %d summaries, want 3", run, len(got))
		}
		for i, w := range want {
			if got[i].Title != w {
				t.Fatalf("run %d: position %d = %q, want %q", run, i, got[i].Title, w)
			}
			if got[i].Order != i+1 {
				t.Errorf("run %d: %q has Order %d, want %d", run, got[i].Title, got[i].Order, i+1)
			}
		}
	}
}

// TestToolInvocationRowsAreNotListedAsSteps. Those rows are the engine's record
// of tool calls an agent ALREADY made. Handing them back as instructions tells
// the agent to redo its own history.
func TestToolInvocationRowsAreNotListedAsSteps(t *testing.T) {
	rows := []map[string]any{
		{"id": "t1", "payload": map[string]any{"title": "a real step", "category": "semantic"}},
		{"id": "t2", "payload": map[string]any{"title": "workbenchHost", "category": "toolInvocation"}},
	}
	got := taskSummariesFromRows(rows)
	if len(got) != 1 || got[0].Title != "a real step" {
		t.Fatalf("toolInvocation rows must be excluded, got %+v", got)
	}
}

// TestRenderTaskStepsIsEmptyForNoTasks -- a plan with no tasks must not carry
// an empty hint, which would read to the model as "the plan has no steps".
func TestRenderTaskStepsIsEmptyForNoTasks(t *testing.T) {
	if got := renderTaskSteps(nil); got != "" {
		t.Errorf("renderTaskSteps(nil) = %q, want empty", got)
	}
	_, hints := buildExecutionTurn("p", planExecutionRow{Kind: userGoalPlanKind, Goal: "g"}, nil)
	if _, present := hints["plan_steps"]; present {
		t.Error("plan_steps hint must be ABSENT when there are no tasks, not empty")
	}
}

// TestUserGoalIsNotDrivenByTwoHandlers is the race guard for memql#4688. The
// dispatch and the phase-checkpoint park must not both fire on one running
// transition; if they do, the plan is simultaneously being executed and parked,
// and its terminal status is whichever write lands last.
func TestUserGoalIsNotDrivenByTwoHandlers(t *testing.T) {
	if executableViaApprovedPath(userGoalPlanKind) && !ownedByAnotherDispatcher(userGoalPlanKind) {
		t.Fatal("userGoal is dispatched by handlePlanApprovedForExecution AND still handled by " +
			"HandlePlanUpdated's running branch -- the turn and the phase-checkpoint park race " +
			"each other on the same plan (memql#4688)")
	}
	// Every kind the approved path claims must be excluded there.
	for _, kind := range []string{userGoalPlanKind, produceArtifactPlanKind, "scopeElevation"} {
		if !ownedByAnotherDispatcher(kind) {
			t.Errorf("%q is dispatched by the approved path but not excluded from HandlePlanUpdated", kind)
		}
	}
	// And a decompose-loop kind must still reach HandlePlanUpdated, or the
	// exclusion has swallowed the handler's actual job.
	if ownedByAnotherDispatcher("analyzeFile") {
		t.Error("analyzeFile must still be handled by HandlePlanUpdated")
	}
}
