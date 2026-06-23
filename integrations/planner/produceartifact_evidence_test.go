package planner

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestBuildExecutionTurn_TrustedScaffoldingNotInUntrustedHistory is the
// memql#1102 regression: a benign "create a document about X" produceArtifact
// turn must put the SYSTEM's trusted produce-flow scaffolding on a hint
// (rendered OUTSIDE the agent prompt's untrusted history block), and leave the
// user-role history message as the raw goal only. When the directive leaked
// into the history message the agent's injection guard refused it as an
// embedded "abandon the produce flow" instruction and the deliverable never
// landed.
func TestBuildExecutionTurn_TrustedScaffoldingNotInUntrustedHistory(t *testing.T) {
	const planId = "v1:planner:plan:p1"
	const goal = "create a list of the top 10 most beautiful birds"
	const owner = "v1:identity:user:u1"

	t.Run("produceArtifact: directive on hint, history is raw goal", func(t *testing.T) {
		plan := planExecutionRow{
			ID:           planId,
			Kind:         produceArtifactPlanKind,
			Goal:         goal,
			PartitionId:      "v1:cognition:space:s1",
			OwnerAgentId: "v1:agents:agent:a1",
			RequestedBy:  owner,
		}
		history, hints := buildExecutionTurn(planId, plan)

		// History is exactly the raw user goal -- nothing else. This is the
		// content the agent renders inside [[BEGIN UNTRUSTED CONVERSATION
		// HISTORY]], so it must carry ONLY genuine user content.
		if len(history) != 1 {
			t.Fatalf("want 1 history message, got %d", len(history))
		}
		if history[0].Role != "user" {
			t.Fatalf("history role = %q, want user", history[0].Role)
		}
		if history[0].Content != goal {
			t.Fatalf("history content = %q, want the raw goal %q", history[0].Content, goal)
		}
		// The trusted scaffolding markers must NOT appear in the (untrusted)
		// history message.
		for _, marker := range []string{"PRODUCE THIS DELIVERABLE NOW", "do NOT call produceArtifact", "workbenchHost"} {
			if strings.Contains(history[0].Content, marker) {
				t.Fatalf("trusted scaffolding %q leaked into the untrusted history message: %q", marker, history[0].Content)
			}
		}

		// The directive rides as a hint, rendered as a trusted block by the
		// agent prompt.
		pd, ok := hints["production_directive"]
		if !ok || strings.TrimSpace(pd) == "" {
			t.Fatalf("produceArtifact must set hints[production_directive], got %q (present=%v)", pd, ok)
		}
		if !strings.Contains(pd, "PRODUCE THIS DELIVERABLE NOW") || !strings.Contains(pd, "do NOT call produceArtifact") {
			t.Fatalf("production_directive missing expected scaffolding: %q", pd)
		}
		// Plumbing that must still be present on the produce path.
		if hints["deliverable_surface"] != "workbench" {
			t.Fatalf("deliverable_surface = %q, want workbench", hints["deliverable_surface"])
		}
		if hints["trigger"] != "plan_approved" {
			t.Fatalf("trigger = %q, want plan_approved", hints["trigger"])
		}
		if hints["owner_user_id"] != owner {
			t.Fatalf("owner_user_id = %q, want %q", hints["owner_user_id"], owner)
		}
	})

	t.Run("scopeElevation: no production directive, raw goal in history", func(t *testing.T) {
		plan := planExecutionRow{
			ID:           planId,
			Kind:         "scopeElevation",
			Goal:         goal,
			PartitionId:      "v1:cognition:space:s1",
			OwnerAgentId: "v1:agents:agent:a1",
			RequestedBy:  owner,
		}
		history, hints := buildExecutionTurn(planId, plan)
		if history[0].Content != goal {
			t.Fatalf("history content = %q, want raw goal", history[0].Content)
		}
		if _, ok := hints["production_directive"]; ok {
			t.Fatalf("scopeElevation must NOT carry a production_directive hint")
		}
		if _, ok := hints["deliverable_surface"]; ok {
			t.Fatalf("scopeElevation must NOT carry a deliverable_surface hint")
		}
	})

	t.Run("watchExecution flips the lane", func(t *testing.T) {
		plan := planExecutionRow{
			ID: planId, Kind: produceArtifactPlanKind, Goal: goal,
			PartitionId: "s", OwnerAgentId: "a", RequestedBy: owner, WatchExecution: true,
		}
		_, hints := buildExecutionTurn(planId, plan)
		if hints["execution_lane"] != "interactive" {
			t.Fatalf("watchExecution should set execution_lane=interactive, got %q", hints["execution_lane"])
		}
	})

	// Resume-from-feedback (epic memql#1404 / memql#1405): when the Plan
	// carries a FeedbackResponse (the user's free-text answer stamped by
	// mutationAttachPlanFeedback) it is folded into the history as a SECOND
	// user-role message -- genuine user content, distinct from the system's
	// trusted produce directive which still rides as a hint.
	t.Run("feedback resume: user answer appended as a user-role history message", func(t *testing.T) {
		const feedback = "use metric units and keep it under 200 words"
		plan := planExecutionRow{
			ID:               planId,
			Kind:             produceArtifactPlanKind,
			Goal:             goal,
			PartitionId:          "v1:cognition:space:s1",
			OwnerAgentId:     "v1:agents:agent:a1",
			RequestedBy:      owner,
			FeedbackResponse: feedback,
		}
		history, hints := buildExecutionTurn(planId, plan)
		if len(history) != 2 {
			t.Fatalf("want 2 history messages (goal + feedback), got %d", len(history))
		}
		if history[0].Content != goal {
			t.Fatalf("first history message must be the raw goal, got %q", history[0].Content)
		}
		if history[1].Role != "user" || history[1].Content != feedback {
			t.Fatalf("second history message must be the user feedback {user,%q}, got {%q,%q}",
				feedback, history[1].Role, history[1].Content)
		}
		// The feedback is genuine user content -- it must NOT be smuggled onto
		// a system hint.
		for _, v := range hints {
			if strings.Contains(v, feedback) {
				t.Fatalf("user feedback leaked onto a system hint: %q", v)
			}
		}
	})

	t.Run("no feedback: single goal history message (first-run dispatch)", func(t *testing.T) {
		plan := planExecutionRow{
			ID: planId, Kind: produceArtifactPlanKind, Goal: goal,
			PartitionId: "s", OwnerAgentId: "a", RequestedBy: owner,
		}
		history, _ := buildExecutionTurn(planId, plan)
		if len(history) != 1 {
			t.Fatalf("first-run dispatch must have exactly 1 history message, got %d", len(history))
		}
	})
}

// TestGeneratedOutputCountFromExecuteResult covers the three shape() output
// shapes the planner can get back from queryGeneratedOutputsForPlan:
// single-row -> bare map, multi-row -> []any, empty/absent -> 0.
func TestGeneratedOutputCountFromExecuteResult(t *testing.T) {
	row := map[string]any{"id": "v1:library:generatedOutput:x"}
	cases := []struct {
		name string
		res  any
		want int
	}{
		{"single bare-map row", map[string]any{"data": row}, 1},
		{"multi-row slice", map[string]any{"data": []any{row, row, row}}, 3},
		{"empty slice", map[string]any{"data": []any{}}, 0},
		{"no data key", map[string]any{}, 0},
		{"non-map result", "nope", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := generatedOutputCountFromExecuteResult(tc.res); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestArtifactProducedForPlan is the memql#939 core: the planner gates a
// produceArtifact plan's success on whether a v1:library:generatedOutput row
// actually landed for it. A clean-but-empty production turn (workbench write
// rejected, agent only apologised) must read as "not produced" so the plan is
// honestly marked failed instead of fake-succeeded.
func TestArtifactProducedForPlan(t *testing.T) {
	const planId = "v1:planner:plan:p1"
	const owner = "v1:identity:user:u1"
	row := map[string]any{"id": "v1:library:generatedOutput:x"}

	cases := []struct {
		name         string
		owner        string
		responder    func(query string) (any, error)
		wantProduced bool
		wantQueryOK  bool
		wantQueried  bool
	}{
		{
			name:  "a row exists -> produced",
			owner: owner,
			responder: func(q string) (any, error) {
				return map[string]any{"data": []any{row}}, nil
			},
			wantProduced: true, wantQueryOK: true, wantQueried: true,
		},
		{
			name:  "zero rows -> not produced (honest failure)",
			owner: owner,
			responder: func(q string) (any, error) {
				return map[string]any{"data": []any{}}, nil
			},
			wantProduced: false, wantQueryOK: true, wantQueried: true,
		},
		{
			name:  "query error -> inconclusive",
			owner: owner,
			responder: func(q string) (any, error) {
				return nil, fmt.Errorf("engine boom")
			},
			wantProduced: false, wantQueryOK: false, wantQueried: true,
		},
		{
			name:         "no owner -> inconclusive, no query attempted",
			owner:        "  ",
			responder:    func(q string) (any, error) { return nil, nil },
			wantProduced: false, wantQueryOK: false, wantQueried: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEngine{execResponder: tc.responder}
			p := &PlannerIntegration{logger: testLogger(), engine: fe}

			produced, queryOK := p.artifactProducedForPlan(context.Background(), planId, tc.owner)

			if produced != tc.wantProduced || queryOK != tc.wantQueryOK {
				t.Fatalf("got (produced=%v, queryOK=%v), want (%v, %v)",
					produced, queryOK, tc.wantProduced, tc.wantQueryOK)
			}

			exec, _, _ := fe.snapshot()
			queried := countContains(exec, "queryGeneratedOutputsForPlan") > 0
			if queried != tc.wantQueried {
				t.Fatalf("queried=%v, want %v (calls: %v)", queried, tc.wantQueried, exec)
			}
			if tc.wantQueried && !strings.Contains(strings.Join(exec, "|"), planId) {
				t.Fatalf("expected the lookup to carry planId %q, calls: %v", planId, exec)
			}
		})
	}
}
