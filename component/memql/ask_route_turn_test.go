package memql

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Routed work guidance is about the goal, so it lasts the run's whole life:
// compiling, working, waiting, and after an automatic retry, a repair or
// re-plan, or a person's re-run of a step (2026-10-19 ruling). Only the
// target's own facts decide: this run, no approval, queued or received.
func TestWorkGuidanceTargetLastsTheRunsLife(t *testing.T) {
	turn := AskTurn{ID: "turn", Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "other", RunID: "other-run", Status: routeQueued, Text: "For the other goal."},
		{GoalID: "goal", RunID: "run", Status: routeQueued, Text: "Actually Phoenix."},
	}}}
	for _, c := range []struct {
		name string
		run  map[string]any
	}{
		{"compiling", map[string]any{"id": "run", "status": "compiling", "templateFingerprint": ""}},
		{"working", map[string]any{"id": "run", "status": "running", "templateFingerprint": "weather-plan"}},
		{"waiting on a retry", map[string]any{"id": "run", "status": "waiting", "waitingOn": map[string]any{"kind": "retry"}}},
		{"resumed by an automatic retry", map[string]any{"id": "run", "status": "running", "templateFingerprint": "weather-plan", "rerun": map[string]any{"requestId": "retry-1", "reason": "rerun"}}},
		{"repaired and re-planned", map[string]any{"id": "run", "status": "running", "templateFingerprint": "replanned", "rerun": map[string]any{"requestId": "replan-1", "reason": "replan"}}},
		{"re-run by the person", map[string]any{"id": "run", "status": "running", "templateFingerprint": "weather-plan", "rerun": map[string]any{"requestId": "person-1", "reason": "rerun", "requestedBy": "owner"}}},
		{"named canonically", map[string]any{"id": "v1:work:run:run", "status": "running"}},
	} {
		target := workGuidanceTarget(&turn, c.run)
		require.NotNil(t, target, c.name)
		require.Equal(t, "Actually Phoenix.", target.Text, c.name)
	}
	running := map[string]any{"id": "run", "status": "running"}
	turn.Routed.Targets[1].Status = routeReceived
	require.NotNil(t, workGuidanceTarget(&turn, running), "received is a receipt, not a delivery cursor")
	for _, other := range []AskRouteTarget{
		{GoalID: "goal", RunID: "run", Status: routeAnswered, ApprovalID: "question", Text: "Use Phoenix."},
		{GoalID: "goal", RunID: "run", Status: routeQueued, ApprovalID: "permission", Text: "Delivered through the request."},
		{GoalID: "goal", RunID: "run", Status: routeNotDelivered, Reason: routeReasonLimit, Text: "Over the cap."},
		{GoalID: "goal", RunID: "run", Status: routeNotReceived, Reason: routeReasonEnded, Text: "Too late."},
	} {
		require.Nil(t, workGuidanceTarget(&AskTurn{Routed: &AskRouting{Targets: []AskRouteTarget{other}}}, running), "%+v", other)
	}
	require.Nil(t, workGuidanceTarget(&AskTurn{ID: "plain"}, running), "an ordinary turn carries no guidance")
	require.Nil(t, workGuidanceTarget(&turn, map[string]any{"status": "running"}), "a run with no id reads nothing")
}

// A queued message ends only with its run: one that finished, failed, was
// cancelled or was asked to stop before reading it shows Not received. A
// retry, repair, re-plan or re-run is the same run and changes nothing.
func TestProjectAskRoutingEndsAQueuedMessageOnlyWithItsRun(t *testing.T) {
	runs := map[string]map[string]any{
		"retried":   {"id": "retried", "goalId": "g", "status": "running", "rerun": map[string]any{"requestId": "retry-1", "reason": "rerun"}},
		"replanned": {"id": "replanned", "goalId": "g", "status": "running", "templateFingerprint": "new-plan", "rerun": map[string]any{"requestId": "replan-1", "reason": "replan"}},
		"compiling": {"id": "compiling", "goalId": "g", "status": "compiling"},
		"waiting":   {"id": "waiting", "goalId": "g", "status": "waiting", "waitingOn": map[string]any{"kind": "repair"}},
		"succeeded": {"id": "succeeded", "goalId": "g", "status": "succeeded"},
		"failed":    {"id": "failed", "goalId": "g", "status": "failed"},
		"cancelled": {"id": "cancelled", "goalId": "g", "status": "cancelled"},
		"stopping":  {"id": "stopping", "goalId": "g", "status": "running", "cancelRequested": true},
	}
	readRun := func(id string) (map[string]any, error) { return runs[id], nil }
	for run, want := range map[string]string{
		"retried": routeQueued, "replanned": routeQueued, "compiling": routeQueued, "waiting": routeQueued,
		"succeeded": routeNotReceived, "failed": routeNotReceived, "cancelled": routeNotReceived, "stopping": routeNotReceived, "unreadable": routeNotReceived,
	} {
		routed := &AskRouting{Targets: []AskRouteTarget{{GoalID: "g", RunID: run, Status: routeQueued, Text: "Actually Phoenix."}}}
		require.NoError(t, projectAskRouting(routed, readRun))
		require.Equal(t, want, routed.Targets[0].Status, run)
	}
	received := &AskRouting{Targets: []AskRouteTarget{{GoalID: "g", RunID: "succeeded", Status: routeReceived, Text: "Actually Phoenix."}}}
	require.NoError(t, projectAskRouting(received, readRun))
	require.Equal(t, routeReceived, received.Targets[0].Status, "a receipt stays a receipt after the run ends")
	stops := &AskRouting{Stops: []AskStopQuestion{{GoalID: "g", RunID: "retried"}, {GoalID: "g", RunID: "cancelled"}, {GoalID: "g", RunID: "succeeded", Decision: stopDecisionKept}}}
	require.NoError(t, projectAskRouting(stops, readRun))
	require.Equal(t, []string{"", stopDecisionEnded, stopDecisionKept}, []string{stops.Stops[0].Decision, stops.Stops[1].Decision, stops.Stops[2].Decision})
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
			{GoalID: "goal", RunID: "run", Title: "Weather in Phoenix", Status: routeQueued, Text: "Actually Phoenix"},
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
			map[string]any{"goalId": "goal", "runId": "run", "title": "Weather in Phoenix", "status": "queued", "text": "Actually Phoenix"},
			map[string]any{"goalId": "other", "runId": "waiting", "title": "Hike plan", "status": "notDelivered", "approvalId": "approval", "reason": "review", "text": "Actually Phoenix"},
		},
		"stops":     []any{map[string]any{"goalId": "report", "runId": "report-run", "title": "Tucson report", "decision": "kept"}},
		"reply":     "Switching the weather check to Phoenix.",
		"statement": "What time is it in Phoenix?",
		"clarify":   true,
		"resolved":  true,
		"skipped":   "invalid",
	}, wire["routed"])
	// A transcript saved before the 2026-10-19 ruling may still carry a
	// target's epoch. The turn no longer has one, so the next save drops it.
	var stored AskTurn
	require.NoError(t, json.Unmarshal([]byte(`{"id":"old","routed":{"targets":[{"goalId":"goal","runId":"run","title":"Weather","status":"queued","epoch":"preparing","text":"Actually Phoenix"}]}}`), &stored))
	raw, err = json.Marshal(stored)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "epoch")
	raw, err = json.Marshal(AskTurn{ID: "plain"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "routed", "an ordinary turn carries no routing")
}
