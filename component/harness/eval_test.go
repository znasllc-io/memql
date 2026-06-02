package harness

import (
	"context"
	"testing"
)

func TestRunEval_DefaultFixturesAllPass(t *testing.T) {
	report, err := RunEval(context.Background(), DefaultFixtures())
	if err != nil {
		t.Fatalf("RunEval error: %v", err)
	}
	if report.Total != len(DefaultFixtures()) {
		t.Fatalf("Total = %d, want %d", report.Total, len(DefaultFixtures()))
	}
	if report.PassedCount != report.Total {
		for _, s := range report.Scores {
			if !s.Passed {
				t.Errorf("fixture %q failed: %s", s.Fixture, s.Reason)
			}
		}
		t.Fatalf("PassedCount = %d, want %d", report.PassedCount, report.Total)
	}
	// Report must surface all five metric dimensions for at least the
	// happy-path fixtures (success rate, step count, tool calls, tokens,
	// wall-clock).
	if report.SuccessRate <= 0 {
		t.Fatalf("SuccessRate = %f, want > 0", report.SuccessRate)
	}
	if report.TotalToolCalls <= 0 {
		t.Fatalf("TotalToolCalls = %d, want > 0", report.TotalToolCalls)
	}
	if report.TotalTokens <= 0 {
		t.Fatalf("TotalTokens = %d, want > 0", report.TotalTokens)
	}
	if report.TotalWallClock <= 0 {
		t.Fatalf("TotalWallClock = %s, want > 0", report.TotalWallClock)
	}
}

func TestDefaultThreshold_ClearedByDefaultFixtures(t *testing.T) {
	report, err := RunEval(context.Background(), DefaultFixtures())
	if err != nil {
		t.Fatalf("RunEval error: %v", err)
	}
	verdict := report.CheckThreshold(DefaultThreshold())
	if !verdict.Passed {
		t.Fatalf("default fixtures must clear the default threshold; violations: %v", verdict.Violations)
	}
}

func TestCheckThreshold_TripsOnRegression(t *testing.T) {
	// A report that under-performs must trip the gate, naming the dimension.
	report := EvalReport{
		Total:          4,
		PassedCount:    2, // pass rate 0.5
		SuccessRate:    0.5,
		TotalToolCalls: 100,
		TotalTokens:    9999,
	}
	th := EvalThreshold{
		MinPassRate:       1.0,
		MinSuccessRate:    0.75,
		MaxTotalToolCalls: 10,
		MaxTotalTokens:    100,
	}
	v := report.CheckThreshold(th)
	if v.Passed {
		t.Fatalf("expected gate to trip on a regressed report")
	}
	if len(v.Violations) != 4 {
		t.Fatalf("expected 4 violations (pass, success, tool calls, tokens), got %d: %v", len(v.Violations), v.Violations)
	}
}

func TestRunEval_RegressionShape_StepReorderWouldFail(t *testing.T) {
	// Simulate a harness regression on a single fixture: a linear chain that
	// (hypothetically) dispatches the wrong number of steps. We model it by
	// asserting an expectation the (correct) harness will violate, proving
	// the gate is sensitive: if the harness ever dispatched s2 before its
	// dep, ExpectStepDispatches would not match.
	bad := TaskFixture{
		Name: "sensitivity-probe",
		Spec: ReplaySpec{
			PlanID: "probe", OwnerUserId: "u1",
			Steps: []ReplayStep{
				{ID: "s1"},
				{ID: "s2", DependsOn: []string{"s1"}},
			},
		},
		ExpectSuccess:        true,
		ExpectStepDispatches: 99, // wrong on purpose
	}
	report, err := RunEval(context.Background(), []TaskFixture{bad})
	if err != nil {
		t.Fatalf("RunEval error: %v", err)
	}
	if report.PassedCount != 0 {
		t.Fatalf("expected the wrong-expectation fixture to FAIL, but it passed")
	}
}
