package harness

// eval_fixtures.go holds the fixed eval task set (#589) plus the default
// regression threshold the CI gate enforces. The fixtures are the harness's
// golden DAG shapes -- the cases the reconciler must get right -- so a
// harness change that breaks ordering, terminal-state computation, retry,
// or dead-end handling drops a fixture's score and fails CI.
//
// Keep these deterministic and small: the whole point is a fast, DB-free,
// LLM-free gate that runs on every PR.

// DefaultFixtures returns the standard eval task set. Each fixture targets a
// specific harness behavior:
//
//   - linear-chain      : steps run in strict dependency order.
//   - fan-out-fan-in    : independent steps + a join; the join waits.
//   - single-step       : the trivial happy path.
//   - failing-step      : a failed step fails the plan (no silent success).
//   - dead-end-blocked  : a step blocked behind a failed dep fails the plan.
//   - mixed-success     : a healthy chain alongside a failing branch fails
//     the plan (one bad branch poisons the plan).
func DefaultFixtures() []TaskFixture {
	return []TaskFixture{
		{
			Name:        "single-step",
			Description: "Trivial happy path: one step, plan succeeds.",
			Spec: ReplaySpec{
				PlanID:      "eval:single-step",
				Goal:        "do one thing",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "s1", Title: "the only step"},
				},
			},
			Outcomes: map[string]StepOutcome{
				"s1": {ToolCalls: 1, Tokens: 100},
			},
			ExpectSuccess:        true,
			ExpectStepDispatches: 1,
		},
		{
			Name:        "linear-chain",
			Description: "s1 -> s2 -> s3 run in strict dependency order.",
			Spec: ReplaySpec{
				PlanID:      "eval:linear-chain",
				Goal:        "three steps in a row",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "s1", Title: "first"},
					{ID: "s2", Title: "second", DependsOn: []string{"s1"}},
					{ID: "s3", Title: "third", DependsOn: []string{"s2"}},
				},
			},
			Outcomes: map[string]StepOutcome{
				"s1": {ToolCalls: 1, Tokens: 50},
				"s2": {ToolCalls: 2, Tokens: 75},
				"s3": {ToolCalls: 1, Tokens: 60},
			},
			ExpectSuccess:        true,
			ExpectStepDispatches: 3,
		},
		{
			Name:        "fan-out-fan-in",
			Description: "root -> {a, b} -> join; the join waits for both.",
			Spec: ReplaySpec{
				PlanID:      "eval:fan-out-fan-in",
				Goal:        "parallel branches then a join",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "root", Title: "root"},
					{ID: "a", Title: "branch a", DependsOn: []string{"root"}},
					{ID: "b", Title: "branch b", DependsOn: []string{"root"}},
					{ID: "join", Title: "join", DependsOn: []string{"a", "b"}},
				},
			},
			Outcomes: map[string]StepOutcome{
				"root": {ToolCalls: 1, Tokens: 40},
				"a":    {ToolCalls: 1, Tokens: 40},
				"b":    {ToolCalls: 1, Tokens: 40},
				"join": {ToolCalls: 1, Tokens: 40},
			},
			ExpectSuccess:        true,
			ExpectStepDispatches: 4,
		},
		{
			Name:        "failing-step",
			Description: "A failed step fails the plan (no silent success).",
			Spec: ReplaySpec{
				PlanID:      "eval:failing-step",
				Goal:        "a step that fails",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "s1", Title: "will fail"},
				},
			},
			Outcomes: map[string]StepOutcome{
				"s1": {Fail: true, ToolCalls: 1, Tokens: 10},
			},
			ExpectSuccess:        false,
			ExpectStepDispatches: 1,
		},
		{
			Name:        "dead-end-blocked",
			Description: "A step behind a failed dependency fails the plan.",
			Spec: ReplaySpec{
				PlanID:      "eval:dead-end-blocked",
				Goal:        "downstream step can never run",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "s1", Title: "fails"},
					{ID: "s2", Title: "depends on the failed step", DependsOn: []string{"s1"}},
				},
			},
			Outcomes: map[string]StepOutcome{
				"s1": {Fail: true, ToolCalls: 1, Tokens: 10},
			},
			ExpectSuccess: false,
			// Only s1 is ever dispatched; s2 never becomes ready.
			ExpectStepDispatches: 1,
		},
		{
			Name:        "mixed-success",
			Description: "A healthy chain plus a failing branch fails the plan.",
			Spec: ReplaySpec{
				PlanID:      "eval:mixed-success",
				Goal:        "one good branch, one bad",
				OwnerUserId: "eval-user",
				Steps: []ReplayStep{
					{ID: "good1", Title: "good first"},
					{ID: "good2", Title: "good second", DependsOn: []string{"good1"}},
					{ID: "bad", Title: "bad branch"},
				},
			},
			Outcomes: map[string]StepOutcome{
				"good1": {ToolCalls: 1, Tokens: 30},
				"good2": {ToolCalls: 1, Tokens: 30},
				"bad":   {Fail: true, ToolCalls: 1, Tokens: 10},
			},
			ExpectSuccess: false,
			// good1, good2, bad all dispatch (bad fails); the plan then fails.
			ExpectStepDispatches: 3,
		},
	}
}

// DefaultThreshold is the regression gate the CI eval lane enforces against
// DefaultFixtures. It is deliberately strict: every fixture must match its
// expectation (pass rate 1.0), because the fixtures are deterministic -- any
// drop is a real harness regression, not noise. The success-rate floor is
// derived from the fixtures (the fixtures that EXPECT success), and the
// tool-call / token caps are set just above the suite's known totals so a
// regression that makes the harness chattier also trips.
func DefaultThreshold() EvalThreshold {
	return EvalThreshold{
		// All fixtures are deterministic -- they must all match expectation.
		MinPassRate: 1.0,
		// 3 of 6 fixtures expect plan success (single-step, linear-chain,
		// fan-out-fan-in); the other 3 deliberately fail to exercise the
		// failure / dead-end / mixed paths.
		MinSuccessRate: 3.0 / 6.0,
		// Suite totals over dispatched steps (current: 14 tool calls, 535
		// tokens). Caps sit just above with small headroom so a chattier or
		// step-reordering regression that re-dispatches steps trips the gate
		// without being flaky on the deterministic fixtures.
		MaxTotalToolCalls: 16,
		MaxTotalTokens:    600,
	}
}
