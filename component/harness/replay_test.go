package harness

import (
	"context"
	"testing"
)

func TestReplay_LinearChainReproducesSequence(t *testing.T) {
	spec := ReplaySpec{
		PlanID:      "p1",
		Goal:        "linear",
		OwnerUserId: "u1",
		Steps: []ReplayStep{
			{ID: "s1", Title: "first"},
			{ID: "s2", Title: "second", DependsOn: []string{"s1"}},
			{ID: "s3", Title: "third", DependsOn: []string{"s2"}},
		},
	}
	recorded := []string{"s1", "s2", "s3"}

	res, err := Replay(context.Background(), spec, recorded)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}
	if !res.Deterministic {
		t.Fatalf("expected deterministic replay for a linear chain, got non-det: %s", res.Reason)
	}
	if !res.Reproduced {
		t.Fatalf("expected reproduced; recorded=%v replayed=%v", recorded, res.ReplayedSequence)
	}
	if !sequencesEqual(res.ReplayedSequence, recorded) {
		t.Fatalf("replayed=%v, want %v", res.ReplayedSequence, recorded)
	}
}

func TestReplay_FanOutFlaggedNonDeterministic(t *testing.T) {
	spec := ReplaySpec{
		PlanID:      "p1",
		Goal:        "fan",
		OwnerUserId: "u1",
		Steps: []ReplayStep{
			{ID: "root"},
			{ID: "a", DependsOn: []string{"root"}},
			{ID: "b", DependsOn: []string{"root"}},
			{ID: "join", DependsOn: []string{"a", "b"}},
		},
	}
	// recorded order has a before b; replay (single-threaded) is stable but
	// the wave {a,b} is not DAG-determined, so it must be flagged.
	recorded := []string{"root", "a", "b", "join"}
	res, err := Replay(context.Background(), spec, recorded)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}
	if res.Deterministic {
		t.Fatalf("expected non-deterministic flag for fan-out wave, got deterministic")
	}
	// Advisory comparison is on the canonicalized order, which should match.
	if !res.Reproduced {
		t.Fatalf("expected advisory reproduced=true on canonicalized order; replayed=%v", res.ReplayedSequence)
	}
}

func TestReplay_DivergenceDetected(t *testing.T) {
	spec := ReplaySpec{
		PlanID:      "p1",
		Goal:        "linear",
		OwnerUserId: "u1",
		Steps: []ReplayStep{
			{ID: "s1"},
			{ID: "s2", DependsOn: []string{"s1"}},
		},
	}
	// A wrong recorded sequence must NOT be reported as reproduced.
	recorded := []string{"s2", "s1"}
	res, err := Replay(context.Background(), spec, recorded)
	if err != nil {
		t.Fatalf("Replay error: %v", err)
	}
	if res.Reproduced {
		t.Fatalf("expected reproduced=false for divergent recorded sequence")
	}
}

func TestReplaySpecFromTrace_RoundTrip(t *testing.T) {
	// Build a trace whose steps mirror a small DAG, plus the StepViews
	// carrying the dependsOn the trace events don't.
	steps := []StepView{
		{ID: "s1", PlanID: "p1", Title: "first", OwnerUserId: "u1"},
		{ID: "s2", PlanID: "p1", Title: "second", DependsOn: []string{"s1"}, OwnerUserId: "u1", Input: map[string]any{"k": "v"}},
	}
	tr := Trace{
		PlanID: "p1",
		Goal:   "g",
		Steps: []StepTimeline{
			{StepID: "s1", Title: "first"},
			{StepID: "s2", Title: "second"},
		},
	}
	spec := ReplaySpecFromTrace(tr, steps)
	if spec.PlanID != "p1" || spec.Goal != "g" || spec.OwnerUserId != "u1" {
		t.Fatalf("spec header mismatch: %+v", spec)
	}
	if len(spec.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(spec.Steps))
	}
	if !sequencesEqual(spec.Steps[1].DependsOn, []string{"s1"}) {
		t.Fatalf("s2 dependsOn = %v, want [s1]", spec.Steps[1].DependsOn)
	}
	if spec.Steps[1].Input["k"] != "v" {
		t.Fatalf("s2 input not carried: %v", spec.Steps[1].Input)
	}
}
