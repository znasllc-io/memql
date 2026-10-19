package memql

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A goal still compiling has no plan to hash, so a message sent to it carries
// the sentinel its first execution accepts. Every other state uses the epoch.
func TestGuidanceEpochForPreparingRun(t *testing.T) {
	compiling := map[string]any{"status": "compiling", "automationName": "work.compile", "templateFingerprint": ""}
	require.Equal(t, "preparing", guidanceEpochFor(compiling))
	running := map[string]any{"status": "running", "templateFingerprint": "weather-plan"}
	require.Equal(t, workGuidanceEpoch(running), guidanceEpochFor(running))
	require.NotEqual(t, "preparing", guidanceEpochFor(running))
	waiting := map[string]any{"status": "waiting", "templateFingerprint": "weather-plan", "waitingOn": map[string]any{"kind": "retry"}}
	require.Equal(t, workGuidanceEpoch(waiting), guidanceEpochFor(waiting))
}

// The sentinel reaches the run's FIRST execution, the one compile starts. A
// re-run or a re-plan is another attempt and never inherits old guidance.
func TestTargetAcceptsTheFirstExecutionOfAPreparingTarget(t *testing.T) {
	preparing := AskRouteTarget{RunID: "run", Status: routeQueued, Epoch: guidanceEpochPreparing, Text: "Actually Phoenix"}
	compiled := map[string]any{"id": "run", "status": "running", "templateFingerprint": "weather-plan"}
	require.True(t, targetAccepts(preparing, compiled), "the first execution takes what was sent while it compiled")
	require.True(t, targetAccepts(preparing, map[string]any{"id": "run", "status": "compiling"}))
	rerun := map[string]any{"id": "run", "status": "running", "templateFingerprint": "weather-plan", "rerun": map[string]any{"requestId": "r"}}
	require.False(t, targetAccepts(preparing, rerun), "a re-run is a different execution attempt")

	current := AskRouteTarget{RunID: "run", Status: routeQueued, Epoch: workGuidanceEpoch(compiled), Text: "Actually Phoenix"}
	require.True(t, targetAccepts(current, compiled))
	require.False(t, targetAccepts(current, rerun), "the epoch moves with a re-run")
	require.False(t, targetAccepts(current, map[string]any{"id": "run", "status": "running", "templateFingerprint": "replanned"}), "and with a new plan")
	require.False(t, targetAccepts(AskRouteTarget{RunID: "run", Status: routeQueued}, compiled), "a target with no epoch reaches no execution")
}

func TestActiveWorkRun(t *testing.T) {
	root := func(fields map[string]any) map[string]any {
		run := map[string]any{"id": "run", "goalId": "goal", "status": "running"}
		for key, value := range fields {
			run[key] = value
		}
		return run
	}
	for _, c := range []struct {
		name   string
		run    map[string]any
		active bool
	}{
		{"preparing", root(map[string]any{"status": "compiling"}), true},
		{"working", root(nil), true},
		{"waiting", root(map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval"}}), true},
		{"succeeded", root(map[string]any{"status": "succeeded"}), false},
		{"failed", root(map[string]any{"status": "failed"}), false},
		{"cancelled", root(map[string]any{"status": "cancelled"}), false},
		{"abandoned", root(map[string]any{"status": "abandoned"}), false},
		{"asked to stop", root(map[string]any{"cancelRequested": true}), false},
		{"delegated child", root(map[string]any{"parentRunId": "parent"}), false},
		{"no goal", root(map[string]any{"goalId": ""}), false},
		{"unreadable", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.active, activeWorkRun(c.run))
		})
	}
}

// A person's later message can retitle a goal; intake's title stays on
// classification and is the fallback.
func TestRunTitlePrefersTheRunTitle(t *testing.T) {
	classified := map[string]any{"classification": map[string]any{"workTitle": "Weather in Tucson"}}
	require.Equal(t, "Weather in Tucson", runTitle(classified))
	classified["title"] = "Weather in Phoenix"
	require.Equal(t, "Weather in Phoenix", runTitle(classified))
	require.Empty(t, runTitle(nil))
}

// The routed turn is a wire contract with the OS client: these names are the
// ones it reads, and the explicit guidance fields are gone from the turn.
func TestRoutedTurnWireShape(t *testing.T) {
	turn := AskTurn{ID: "turn", Prompt: "Actually Phoenix", Routed: &AskRouting{
		Targets: []AskRouteTarget{
			{GoalID: "goal", RunID: "run", Title: "Weather in Phoenix", Status: routeQueued, Epoch: guidanceEpochPreparing, Text: "Actually Phoenix"},
			{GoalID: "other", RunID: "waiting", Title: "Hike plan", Status: routeNotDelivered, ApprovalID: "approval", Reason: routeReasonReview, Text: "Actually Phoenix"},
		},
		Stops:     []AskStopQuestion{{GoalID: "report", RunID: "report-run", Title: "Tucson report", Decision: stopDecisionKept}},
		Reply:     "Switching the weather check to Phoenix.",
		Statement: "What time is it in Phoenix?",
		Clarify:   true,
		Resolved:  true,
		Skipped:   "invalid",
	}}
	raw, err := json.Marshal(turn)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	for _, removed := range []string{"canGuideWork", "guidanceTargetRunId", "guidanceStatus", "guidanceEpoch"} {
		require.NotContains(t, wire, removed)
	}
	require.Equal(t, map[string]any{
		"targets": []any{
			map[string]any{"goalId": "goal", "runId": "run", "title": "Weather in Phoenix", "status": "queued", "epoch": "preparing", "text": "Actually Phoenix"},
			map[string]any{"goalId": "other", "runId": "waiting", "title": "Hike plan", "status": "notDelivered", "approvalId": "approval", "reason": "review", "text": "Actually Phoenix"},
		},
		"stops":     []any{map[string]any{"goalId": "report", "runId": "report-run", "title": "Tucson report", "decision": "kept"}},
		"reply":     "Switching the weather check to Phoenix.",
		"statement": "What time is it in Phoenix?",
		"clarify":   true,
		"resolved":  true,
		"skipped":   "invalid",
	}, wire["routed"])
	raw, err = json.Marshal(AskTurn{ID: "plain"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "routed", "an ordinary turn carries no routing")
}
