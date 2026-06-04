package planner

import "testing"

// The cumulative per-plan cap default is dropped to a small number
// (memql#843) so a misbehaving plan parks within a handful of calls.
func TestDefaultMaxPlannerInvocationsPerPlan_IsLow(t *testing.T) {
	if defaultMaxPlannerInvocationsPerPlan > 8 {
		t.Fatalf("per-plan invocation cap default must be small (<=8), got %d", defaultMaxPlannerInvocationsPerPlan)
	}
}

// A plan whose persisted llmCallCount has reached the cap is blocked
// before the next call -- i.e. it parks within a handful of calls.
func TestPlannerCallGate_ParksAtLoweredCap(t *testing.T) {
	capN := maxPlannerInvocationsPerPlan() // honors env; defaults to 8
	// Just under the cap -> allowed.
	planUnder := map[string]any{"metrics": map[string]any{"llmCallCount": capN - 1}}
	if g := evaluatePlannerCallGate(planUnder, 100, capN, 2_000_000); g.Blocked {
		t.Fatalf("a plan one call under the cap must still be allowed")
	}
	// At the cap -> blocked (no further LLM call).
	planAt := map[string]any{"metrics": map[string]any{"llmCallCount": capN}}
	if g := evaluatePlannerCallGate(planAt, 100, capN, 2_000_000); !g.Blocked {
		t.Fatalf("a plan at the cap (%d) must be blocked", capN)
	}
}

// The convergence guard now tracks repeated no-task markPlanSucceeded
// (previously untracked): it parks once the same threshold is exceeded.
func TestConvTracker_NoTaskSucceedParks(t *testing.T) {
	c := newConvTracker()
	threshold := maxIdenticalNonTerminalDecisions() // default 2
	// First `threshold` emissions are allowed (re-invoke).
	for i := 1; i <= threshold; i++ {
		if park, _ := c.recordNoTaskSucceedAndCheck(); park {
			t.Fatalf("no-task succeed #%d should not park yet (threshold %d)", i, threshold)
		}
	}
	// The one past the threshold parks.
	if park, count := c.recordNoTaskSucceedAndCheck(); !park {
		t.Fatalf("no-task succeed past the threshold must park (count=%d)", count)
	}
}

// A nil tracker is safe (never panics, never parks).
func TestConvTracker_NoTaskSucceed_NilSafe(t *testing.T) {
	var c *convTracker
	if park, _ := c.recordNoTaskSucceedAndCheck(); park {
		t.Fatalf("nil tracker must not park")
	}
}
