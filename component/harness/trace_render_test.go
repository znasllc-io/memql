package harness

import (
	"strings"
	"testing"
	"time"
)

func TestTraceRender_ContainsHeaderAndTimeline(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	events := []TraceEvent{
		{At: at(base, 0), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusOpen, Mutation: "mutationCreateHarnessPlan", Actor: "u1", Content: "the goal"},
		{At: at(base, 1), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusRunning, Mutation: "mutationStartHarnessStep", Actor: "system", Content: "do it"},
		{At: at(base, 2), Kind: EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "tool_result", Actor: "system", Content: "ran a tool", Data: map[string]any{"toolName": "search"}},
		{At: at(base, 3), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusDone, Mutation: "harnessReconciler.setPlanStatus", Actor: "system", Content: "the goal"},
	}
	out := AssembleTrace("p1", events).Render()

	for _, want := range []string{
		"Plan p1",
		"goal:   the goal",
		"status: done",
		"Timeline:",
		"[plan]",
		"[step]",
		"[obs]",
		"tool=search",
		"mutationStartHarnessStep",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
}

func TestTraceRender_EmptyTrace(t *testing.T) {
	out := AssembleTrace("missing", nil).Render()
	if !strings.Contains(out, "no timeline events") {
		t.Fatalf("empty render should note no events:\n%s", out)
	}
}
