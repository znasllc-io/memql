package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"maps"
)

func TestRunAskContinuesFinishedOpenGoalAndKeepsGuidance(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "reply": "I finished everything."}))
	goal, run := f.goal("draft", "Prepare and deliver the report", "Report", "succeeded")
	f.write("createWorkGoal", map[string]any{"goalId": goal, "statement": "Prepare and deliver the report", "origin": "user", "requestedVia": "ask"})
	f.record(AskTurn{ID: "earlier", Prompt: "Keep the sources", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: goal, RunID: run, Status: routeReceived, Text: "Keep the sources"}}}})
	var calls []map[string]any
	f.e.builtinExecutorHandlers["integration.work.continueRun"] = func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.unlocked("continueRun")
		f.checkCaller(ctx, "continueRun")
		calls = append(calls, args)
		f.write("updateWorkRun", map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "replan", "subject": "draft"}})
		return askValue(map[string]any{"runId": run, "continuationRequested": true})
	}
	_, answer, err := f.ask("continue", "Deliver the report from the saved work", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Continuation requested for Report.", answer, "use the observed disposition, never the model's success claim")
	require.Empty(t, f.intake)
	require.Len(t, calls, 1)
	require.Equal(t, run, calls[0]["runId"])
	require.NotEmpty(t, calls[0]["runVersion"])
	require.Equal(t, "Deliver the report from the saved work", calls[0]["feedback"])
	f.write("updateWorkRun", map[string]any{"runId": run, "feedbackContinuation": resultGuidanceReceipt(run, stringArg(calls[0], "requestId"), f.user, stringArg(calls[0], "feedback"), time.Now().UTC())})
	f.write("updateWorkRun", map[string]any{"runId": run, "status": "running", "waitingOn": map[string]any{}})
	require.Equal(t, []WorkGuidance{{ID: "earlier", Prompt: "Keep the sources"}, {ID: BareShortId(WorkResultFeedbackID(run, stringArg(calls[0], "requestId"))), Prompt: "Deliver the report from the saved work"}}, f.guidance(goal, run))
	require.Empty(t, f.held)
	_, _, err = f.ask("continue", "Deliver the report from the saved work", AskRoute{})
	require.NoError(t, err)
	require.Len(t, calls, 1, "resent Ask turn reuses its recorded delivery")
}

func TestAskResultFeedbackReceiptSurvivesMissingTranscriptDelivery(t *testing.T) {
	f := newRouteFixture(t)
	goal, run := f.goal("draft", "Deliver the report", "Report", "running")
	f.record(AskTurn{ID: "earlier", Prompt: "Keep the sources", StartedAt: time.Now().Add(-time.Minute), Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: goal, RunID: run, Status: routeReceived, Text: "Keep the sources"}}}})
	f.write("updateWorkRun", map[string]any{"runId": run, "feedbackContinuation": resultGuidanceReceipt(run, "missing-ask-delivery", f.user, "Deliver the saved result", time.Now().UTC())})
	guidance := f.guidance(goal, run)
	require.Len(t, guidance, 2)
	require.Equal(t, "Keep the sources", guidance[0].Prompt)
	require.Equal(t, "Deliver the saved result", guidance[1].Prompt)
	require.Contains(t, guidance[1].ID, "result-feedback-")
}

func TestAskOpenFinishedGoalsStillRouteUnrelatedAndAmbiguousMessages(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			decision := map[string]any{}
			if ambiguous {
				decision = map[string]any{"clarify": "The report or the map?", "reply": "The report or the map?"}
			}
			f := newRouteFixture(t, routeAnswer(decision))
			for _, label := range []string{"report", "map"} {
				goal, _ := f.goal(label, "Prepare the "+label, label, "succeeded")
				f.write("createWorkGoal", map[string]any{"goalId": goal, "statement": "Prepare the " + label, "origin": "user", "requestedVia": "ask"})
			}
			calls := 0
			f.e.builtinExecutorHandlers["integration.work.continueRun"] = func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				calls++
				return nil, fmt.Errorf("must not continue an unselected goal")
			}
			prompt := "What time is it?"
			if ambiguous {
				prompt = "Fix that one"
			}
			_, answer, err := f.ask("next", prompt, AskRoute{})
			require.NoError(t, err)
			require.Zero(t, calls)
			if ambiguous {
				require.Empty(t, f.intake)
				require.Equal(t, "The report or the map?", answer)
			} else {
				require.Equal(t, []string{prompt}, f.intake)
			}
		})
	}
}

func TestRunAskContinuationRefusalDoesNotCreateSecondGoal(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "reply": "Continuing now."}))
	goal, _ := f.goal("draft", "Deliver the report", "Report", "succeeded")
	f.write("createWorkGoal", map[string]any{"goalId": goal, "statement": "Deliver the report", "origin": "user", "requestedVia": "ask"})
	f.e.builtinExecutorHandlers["integration.work.continueRun"] = func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		return nil, fmt.Errorf("result changed while routing")
	}
	_, answer, err := f.ask("continue", "Finish its delivery", AskRoute{})
	require.NoError(t, err)
	require.Contains(t, answer, "couldn't take this message")
	require.Empty(t, f.intake)
	require.Equal(t, routeNotDelivered, f.turn("continue").Routed.Targets[0].Status)
}

func TestAskFinishedGoalDiscoveryKeepsActiveProjectionSeparate(t *testing.T) {
	f := newRouteFixture(t)
	goal, run := f.goal("draft", "Deliver the report", "Report", "succeeded")
	f.write("createWorkGoal", map[string]any{"goalId": goal, "statement": "Deliver the report", "origin": "user", "requestedVia": "ask"})
	transcript := askTranscript{Turns: []AskTurn{f.turn("draft")}}
	active, err := f.e.activeAskGoals(f.ctx, transcript)
	require.NoError(t, err)
	require.Empty(t, active)
	eligible, err := f.e.routableAskGoals(f.ctx, transcript)
	require.NoError(t, err)
	require.Len(t, eligible, 1)
	require.NotEmpty(t, eligible[0].ContinueVersion)
	f.write("updateWorkGoal", map[string]any{"goalId": goal, "status": "closed", "acceptance": map[string]any{"runId": run}})
	eligible, err = f.e.routableAskGoals(f.ctx, transcript)
	require.NoError(t, err)
	require.Empty(t, eligible)
}

func resultGuidanceReceipt(run, request, owner, text string, at time.Time) map[string]any {
	return map[string]any{"schemaVersion": 1, "id": WorkResultFeedbackID(run, request), "requestId": request, "feedback": text, "requestedBy": owner, "requestedAt": at.Format(time.RFC3339Nano)}
}

func TestAskResultFeedbackKeepsCursorAcrossSealTranscriptAndLaterAPIRequests(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	runtime := guidanceRunContext(ctx, run)
	at := time.Now().UTC().Add(-time.Minute)
	receipt := resultGuidanceReceipt(run, BareShortId(conversation)+":continue", actor.UserId, "Deliver the saved report.", at)
	write("updateWorkRun", map[string]any{"runId": run, "feedbackContinuation": receipt})
	page, err := b.ReadWorkGuidancePage(runtime, WorkGuidanceCursor{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	require.NoError(t, b.AcknowledgeWorkGuidancePage(runtime, page.Next))
	seal := func(receipt map[string]any, accepted bool) {
		data := maps.Clone(receipt)
		data["accepted"] = accepted
		write("createWorkObservation", map[string]any{"observationId": data["id"], "runId": run, "kind": "decision", "content": "Owner feedback", "data": map[string]any{"resultFeedback": data}})
	}
	seal(receipt, true)
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "continue", Prompt: "Deliver the saved report.", StartedAt: time.Now(), Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeReceived, Text: "Deliver the saved report."}}}})
	again, err := a.ReadWorkGuidancePage(runtime, page.Next)
	require.NoError(t, err)
	require.False(t, again.Reset)
	require.Empty(t, again.Messages, "the late transcript must not rename or reorder consumed feedback")
	// Earlier API-only feedback remains durable after the latest run receipt
	// advances. Pending intents without a committed run receipt stay hidden.
	pending := resultGuidanceReceipt(run, "uncommitted", actor.UserId, "Never dispatched.", at.Add(time.Second))
	seal(pending, false)
	var expected []WorkGuidance
	expected = append(expected, page.Messages...)
	for n := 0; n < 20; n++ {
		next := resultGuidanceReceipt(run, fmt.Sprintf("api-%02d", n), actor.UserId, fmt.Sprintf("Refine section %d.", n), at.Add(time.Duration(n+2)*time.Second))
		seal(next, true)
		write("updateWorkRun", map[string]any{"runId": run, "feedbackContinuation": next})
		expected = append(expected, WorkGuidance{ID: BareShortId(stringArg(next, "id")), Prompt: stringArg(next, "feedback")})
	}
	all, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, expected, all, "all accepted receipts survive the 17-row query page")
	rest, err := b.ReadWorkGuidancePage(runtime, page.Next)
	require.NoError(t, err)
	require.False(t, rest.Reset)
	require.Equal(t, expected[1:1+len(rest.Messages)], rest.Messages)
}
