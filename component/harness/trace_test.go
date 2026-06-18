package harness

import (
	"testing"
	"time"
)

// helper to build a timestamp at a fixed base + offset seconds.
func at(base time.Time, sec int) time.Time {
	return base.Add(time.Duration(sec) * time.Second)
}

func TestAssembleTrace_OrdersByCreatedAt(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Deliberately out of order on input.
	events := []TraceEvent{
		{At: at(base, 5), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusDone, Mutation: "mutationCompleteHarnessStep"},
		{At: at(base, 0), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusOpen, Mutation: "mutationCreateHarnessPlan", Content: "the goal"},
		{At: at(base, 3), Kind: EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "tool_result", Content: "did a thing", Data: map[string]any{"toolName": "search"}},
		{At: at(base, 2), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusRunning, Mutation: "mutationStartHarnessStep"},
		{At: at(base, 1), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusReady, Mutation: "mutationReadyHarnessStep"},
		{At: at(base, 6), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusDone, Mutation: "harnessReconciler.setPlanStatus", Content: "the goal"},
	}

	tr := AssembleTrace("p1", events)

	if tr.PlanID != "p1" {
		t.Fatalf("PlanID = %q, want p1", tr.PlanID)
	}
	if tr.Goal != "the goal" {
		t.Fatalf("Goal = %q, want 'the goal'", tr.Goal)
	}
	if tr.FinalStatus != PlanStatusDone {
		t.Fatalf("FinalStatus = %q, want done", tr.FinalStatus)
	}
	// Verify ascending order.
	for i := 1; i < len(tr.Events); i++ {
		if tr.Events[i].At.Before(tr.Events[i-1].At) {
			t.Fatalf("events not ordered ascending at index %d", i)
		}
	}
	if !tr.IsComplete() {
		t.Fatalf("IsComplete() = false, want true for done plan")
	}
}

func TestAssembleTrace_RollupSteps(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	events := []TraceEvent{
		{At: at(base, 1), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusReady, Content: "step one"},
		{At: at(base, 2), Kind: EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "note", Content: "n"},
		{At: at(base, 3), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusDone},
		{At: at(base, 4), Kind: EventKindStep, NodeID: "s2", StepID: "s2", Status: StepStatusFailed, Content: "step two"},
	}
	tr := AssembleTrace("p1", events)
	if len(tr.Steps) != 2 {
		t.Fatalf("got %d step timelines, want 2", len(tr.Steps))
	}
	// first-seen order preserved
	if tr.Steps[0].StepID != "s1" || tr.Steps[1].StepID != "s2" {
		t.Fatalf("step order = %q,%q want s1,s2", tr.Steps[0].StepID, tr.Steps[1].StepID)
	}
	if tr.Steps[0].Title != "step one" {
		t.Fatalf("s1 title = %q", tr.Steps[0].Title)
	}
	if tr.Steps[0].FinalStatus != StepStatusDone {
		t.Fatalf("s1 final status = %q, want done", tr.Steps[0].FinalStatus)
	}
	if len(tr.Steps[0].Observations) != 1 {
		t.Fatalf("s1 obs count = %d, want 1", len(tr.Steps[0].Observations))
	}
	if tr.Steps[1].FinalStatus != StepStatusFailed {
		t.Fatalf("s2 final status = %q, want failed", tr.Steps[1].FinalStatus)
	}
}

func TestReplaySequence_OrderOfRunningTransitions(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	events := []TraceEvent{
		{At: at(base, 1), Kind: EventKindStep, NodeID: "a", StepID: "a", Status: StepStatusRunning},
		{At: at(base, 2), Kind: EventKindStep, NodeID: "b", StepID: "b", Status: StepStatusRunning},
		{At: at(base, 3), Kind: EventKindStep, NodeID: "a", StepID: "a", Status: StepStatusDone},
		{At: at(base, 4), Kind: EventKindStep, NodeID: "c", StepID: "c", Status: StepStatusRunning},
	}
	tr := AssembleTrace("p1", events)
	seq := tr.ReplaySequence()
	want := []string{"a", "b", "c"}
	if !sequencesEqual(seq, want) {
		t.Fatalf("ReplaySequence = %v, want %v", seq, want)
	}
}

func TestComputeMetrics_FromTrace(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	events := []TraceEvent{
		{At: at(base, 0), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusOpen, Content: "g"},
		{At: at(base, 1), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusRunning},
		{At: at(base, 2), Kind: EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "tool_result", Data: map[string]any{"toolCalls": float64(3), "tokens": float64(120)}},
		{At: at(base, 3), Kind: EventKindStep, NodeID: "s1", StepID: "s1", Status: StepStatusDone},
		{At: at(base, 4), Kind: EventKindStep, NodeID: "s2", StepID: "s2", Status: StepStatusFailed},
		{At: at(base, 5), Kind: EventKindObservation, NodeID: "o2", StepID: "s2", ObservationKind: "error", Content: "boom"},
		{At: at(base, 10), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusFailed, Content: "g"},
	}
	tr := AssembleTrace("p1", events)
	m := ComputeMetrics(tr)

	if m.Success {
		t.Fatalf("Success = true, want false (plan failed)")
	}
	if m.StepCount != 2 {
		t.Fatalf("StepCount = %d, want 2", m.StepCount)
	}
	if m.StepsCompleted != 1 || m.StepsFailed != 1 {
		t.Fatalf("StepsCompleted=%d StepsFailed=%d, want 1,1", m.StepsCompleted, m.StepsFailed)
	}
	if m.ToolCalls != 3 {
		t.Fatalf("ToolCalls = %d, want 3", m.ToolCalls)
	}
	if m.TokenCost != 120 {
		t.Fatalf("TokenCost = %d, want 120", m.TokenCost)
	}
	if m.ObservationCount != 2 || m.ErrorCount != 1 {
		t.Fatalf("ObservationCount=%d ErrorCount=%d, want 2,1", m.ObservationCount, m.ErrorCount)
	}
	if m.WallClock != 10*time.Second {
		t.Fatalf("WallClock = %s, want 10s", m.WallClock)
	}
}

func TestComputeMetrics_ToolResultCountsAsOneCall(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	events := []TraceEvent{
		{At: at(base, 0), Kind: EventKindPlan, NodeID: "p1", Status: PlanStatusDone, Content: "g"},
		{At: at(base, 1), Kind: EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "tool_result"},
		{At: at(base, 2), Kind: EventKindObservation, NodeID: "o2", StepID: "s1", ObservationKind: "tool_result"},
	}
	m := ComputeMetrics(AssembleTrace("p1", events))
	if m.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want 2 (each tool_result is one call)", m.ToolCalls)
	}
	if !m.Success {
		t.Fatalf("Success = false, want true (plan done)")
	}
}
