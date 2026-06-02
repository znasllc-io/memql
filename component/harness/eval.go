package harness

// eval.go is the harness eval scaffold (#589): a fixed set of task fixtures
// run against the SAME reconciler decision core, scored on task success,
// step count, tool-call count, token cost, and wall-clock. The runner
// executes every fixture over an in-memory graph (no Postgres, no LLM), so
// it runs as a fast CI gate that fails on a configured regression
// threshold.
//
// Why this catches regressions: harness changes (the reconciler #583, the
// inner loop #584, the planner #587) all flow through the same
// SelectRunnable / PromotablePending / ComputePlanTerminal core the eval
// drives. A change that, say, dispatches a step out of dependency order or
// fails a plan that should succeed shows up immediately as a fixture score
// drop. The fixtures model the DAG shapes the harness must get right
// (linear chains, fan-out/fan-in, retry, dead-end blocking) plus scripted
// tool outcomes, so the eval measures the harness, not the LLM.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// TaskFixture is one eval task: a plan recipe plus the scripted tool
// outcomes for its steps and the expected result. The fixture is pure data,
// so the suite is reproducible and reviewable.
type TaskFixture struct {
	// Name is the fixture's stable identifier (used in reports + thresholds).
	Name string
	// Description explains what harness behavior the fixture exercises.
	Description string
	// Spec is the plan DAG to run.
	Spec ReplaySpec
	// Outcomes maps step id -> scripted dispatch outcome. A step with no
	// entry succeeds with zero tool calls. This is how a fixture models a
	// failing tool, a retry, or token usage deterministically.
	Outcomes map[string]StepOutcome
	// ExpectSuccess is the expected plan-level success (status==done).
	ExpectSuccess bool
	// ExpectStepDispatches is the expected number of step dispatches. Zero
	// means "do not assert" (used for fixtures whose count is incidental).
	ExpectStepDispatches int
}

// StepOutcome scripts a single step's dispatch result for a fixture.
type StepOutcome struct {
	// Fail makes the step's dispatch return an error (step -> failed).
	Fail bool
	// ToolCalls is the tool-call count the dispatch reports.
	ToolCalls int
	// Tokens is the token cost the dispatch reports.
	Tokens int
	// Result is the step's result payload on success.
	Result map[string]any
}

// EvalScore is one fixture's measured outcome.
type EvalScore struct {
	Fixture        string
	Success        bool
	ExpectSuccess  bool
	Passed         bool // measured behavior matched the fixture's expectation
	StepCount      int
	StepDispatches int
	ToolCalls      int
	TokenCost      int
	WallClock      time.Duration
	Reason         string // populated when Passed is false
}

// EvalReport is the suite-level rollup the runner returns and the CI gate
// checks.
type EvalReport struct {
	Scores         []EvalScore
	Total          int
	PassedCount    int
	SuccessRate    float64 // fraction of fixtures whose plan succeeded
	TotalToolCalls int
	TotalTokens    int
	TotalWallClock time.Duration
}

// RunEval runs every fixture and returns the suite report. Each fixture is
// driven through the reconciler over an in-memory graph with the fixture's
// scripted dispatcher, then scored from the resulting state. No DB / LLM.
func RunEval(ctx context.Context, fixtures []TaskFixture) (EvalReport, error) {
	report := EvalReport{Total: len(fixtures)}
	for _, fx := range fixtures {
		score, err := runFixture(ctx, fx)
		if err != nil {
			return EvalReport{}, fmt.Errorf("fixture %q: %w", fx.Name, err)
		}
		report.Scores = append(report.Scores, score)
		if score.Passed {
			report.PassedCount++
		}
		if score.Success {
			report.SuccessRate++
		}
		report.TotalToolCalls += score.ToolCalls
		report.TotalTokens += score.TokenCost
		report.TotalWallClock += score.WallClock
	}
	if report.Total > 0 {
		report.SuccessRate /= float64(report.Total)
	}
	sort.Slice(report.Scores, func(i, j int) bool {
		return report.Scores[i].Fixture < report.Scores[j].Fixture
	})
	return report, nil
}

// runFixture drives one fixture to a fixed point and scores it.
func runFixture(ctx context.Context, fx TaskFixture) (EvalScore, error) {
	start := time.Now()
	graph := newReplayGraph(fx.Spec).withDispatch(scriptedDispatch(fx.Outcomes))

	rec, err := New(
		graph, graph, graph, graph, nil,
		slog.New(slog.NewTextHandler(noopWriter{}, nil)),
		Config{MaxConcurrentPerPartition: 1},
	)
	if err != nil {
		return EvalScore{}, err
	}

	const maxTicks = 1000
	for i := 0; i < maxTicks; i++ {
		before := graph.dispatchCount()
		if err := rec.Reconcile(ctx, fx.Spec.PlanID); err != nil {
			return EvalScore{}, err
		}
		status, _ := graph.PlanStatus(ctx, fx.Spec.PlanID)
		if isTerminalPlanStatus(status) {
			break
		}
		if graph.dispatchCount() == before {
			break
		}
	}

	status, _ := graph.PlanStatus(ctx, fx.Spec.PlanID)
	success := status == PlanStatusDone
	dispatches := graph.dispatchOrder()

	toolCalls, tokens := tallyOutcomes(dispatches, fx.Outcomes)

	score := EvalScore{
		Fixture:        fx.Name,
		Success:        success,
		ExpectSuccess:  fx.ExpectSuccess,
		StepCount:      len(fx.Spec.Steps),
		StepDispatches: len(dispatches),
		ToolCalls:      toolCalls,
		TokenCost:      tokens,
		WallClock:      time.Since(start),
	}
	score.Passed, score.Reason = scoreFixture(fx, score)
	return score, nil
}

// scoreFixture compares measured behavior to the fixture's expectations.
func scoreFixture(fx TaskFixture, s EvalScore) (bool, string) {
	if s.Success != fx.ExpectSuccess {
		return false, fmt.Sprintf("expected success=%v, got success=%v (status mismatch)", fx.ExpectSuccess, s.Success)
	}
	if fx.ExpectStepDispatches > 0 && s.StepDispatches != fx.ExpectStepDispatches {
		return false, fmt.Sprintf("expected %d step dispatch(es), got %d", fx.ExpectStepDispatches, s.StepDispatches)
	}
	return true, ""
}

// tallyOutcomes sums the scripted tool-call + token cost over the steps that
// were actually dispatched.
func tallyOutcomes(dispatched []string, outcomes map[string]StepOutcome) (toolCalls, tokens int) {
	for _, id := range dispatched {
		o := outcomes[id]
		toolCalls += o.ToolCalls
		tokens += o.Tokens
	}
	return toolCalls, tokens
}

// scriptedDispatch builds a ReplayDispatch that returns each step's scripted
// outcome (failing, tool-call count, token cost, result).
func scriptedDispatch(outcomes map[string]StepOutcome) ReplayDispatch {
	return func(_ context.Context, step StepView) (map[string]any, int, error) {
		o := outcomes[step.ID]
		if o.Fail {
			return nil, o.ToolCalls, fmt.Errorf("scripted failure for step %q", step.ID)
		}
		result := o.Result
		if result == nil {
			result = map[string]any{"ok": true}
		}
		return result, o.ToolCalls, nil
	}
}

// ---------------------------------------------------------------------------
// Regression gate
// ---------------------------------------------------------------------------

// EvalThreshold configures the CI regression gate. The eval fails when the
// suite drops below any configured floor. Zero-value fields are not
// enforced, so a caller can gate on just the dimensions it cares about.
type EvalThreshold struct {
	// MinSuccessRate is the minimum acceptable fraction of fixtures whose
	// plan succeeded (0..1). A drop trips the gate.
	MinSuccessRate float64
	// MinPassRate is the minimum acceptable fraction of fixtures that matched
	// their expectation (success AND step-count). This is the primary gate:
	// it catches a harness change that, say, reorders dispatch or mis-fails a
	// plan even when the raw success rate is unchanged.
	MinPassRate float64
	// MaxTotalToolCalls caps the cumulative tool calls across the suite; a
	// regression that makes the harness chattier trips this.
	MaxTotalToolCalls int
	// MaxTotalTokens caps the cumulative token cost across the suite.
	MaxTotalTokens int
}

// GateVerdict reports whether the suite cleared the threshold.
type GateVerdict struct {
	Passed     bool
	Violations []string
}

// CheckThreshold evaluates a report against a threshold, returning every
// violated dimension so the CI log names exactly what regressed.
func (rep EvalReport) CheckThreshold(th EvalThreshold) GateVerdict {
	v := GateVerdict{Passed: true}
	passRate := 0.0
	if rep.Total > 0 {
		passRate = float64(rep.PassedCount) / float64(rep.Total)
	}
	if th.MinPassRate > 0 && passRate < th.MinPassRate {
		v.Passed = false
		v.Violations = append(v.Violations,
			fmt.Sprintf("pass rate %.3f below floor %.3f", passRate, th.MinPassRate))
	}
	if th.MinSuccessRate > 0 && rep.SuccessRate < th.MinSuccessRate {
		v.Passed = false
		v.Violations = append(v.Violations,
			fmt.Sprintf("success rate %.3f below floor %.3f", rep.SuccessRate, th.MinSuccessRate))
	}
	if th.MaxTotalToolCalls > 0 && rep.TotalToolCalls > th.MaxTotalToolCalls {
		v.Passed = false
		v.Violations = append(v.Violations,
			fmt.Sprintf("total tool calls %d exceeds cap %d", rep.TotalToolCalls, th.MaxTotalToolCalls))
	}
	if th.MaxTotalTokens > 0 && rep.TotalTokens > th.MaxTotalTokens {
		v.Passed = false
		v.Violations = append(v.Violations,
			fmt.Sprintf("total tokens %d exceeds cap %d", rep.TotalTokens, th.MaxTotalTokens))
	}
	return v
}
