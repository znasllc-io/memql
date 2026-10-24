package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestCompletedFormatReceiptRecoversOnlyOneOwnedAttemptOnPeer(t *testing.T) {
	a, b, ctx, _, runID, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Save the requested output", "origin": "user"})
	runtime := guidanceRunContext(ctx, runID)
	run, _ := common.RunFromContext(runtime)
	write("createWorkStep", map[string]any{"stepId": runID + "-" + run.StepKey, "runId": runID, "key": run.StepKey, "seq": 0, "attempt": 1, "version": 1, "stepType": "function", "status": "running"})
	state, err := a.ReadWorkReplyReview(runtime)
	require.NoError(t, err)
	input := WorkDelegatedOutcome{SessionID: "operational-session", Draft: "saved native response", AgentID: "assistant", AgentRole: "assistant"}
	pending, err := json.Marshal(map[string]any{"outcomeFormat": map[string]any{"input": input, "attempts": 1, "done": false}})
	require.NoError(t, err)
	answer := `{"status":"complete","response":"Retained evidence is ready for review.","question":null}`
	receipt := func(step, prompt, body string) {
		write("createWorkModelCall", map[string]any{"modelCallId": id.NewShortId(), "runId": runID, "stepKey": step, "requestHash": strings.Repeat("a", 64), "provider": "app:codex", "model": "fixture", "served": "subscription", "promptRef": prompt, "response": map[string]any{"text": body}})
	}
	receipt(run.StepKey, "workOutcomeFormat", answer) // Earlier attempts cannot be borrowed.
	require.NoError(t, a.SaveWorkReplyReview(runtime, state.Binding, pending))
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.ErrorContains(t, err, "missing or ambiguous")
	receipt("other-step", "workOutcomeFormat", answer)
	receipt(run.StepKey, "workReplyReview", answer)
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.ErrorContains(t, err, "missing or ambiguous")
	receipt(run.StepKey, "workOutcomeFormat", answer)
	actor, _ := auth.AccessFromContext(ctx)
	recordApprovalGuidance(write, actor.UserId, runID, "Use the corrected evidence title")
	got, err := b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.NoError(t, err)
	require.Equal(t, answer, got)
	changed := input
	changed.SessionID = "other-production"
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, changed)
	require.ErrorContains(t, err, "input changed")
	foreign := common.ContextWithRun(auth.ContextWithUserActor(ctx, "stranger"), run)
	_, err = b.ReadCompletedWorkOutcomeFormat(foreign, input)
	require.Error(t, err)
	receipt(run.StepKey, "workOutcomeFormat", answer)
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.ErrorContains(t, err, "missing or ambiguous")
	write("updateWorkStep", map[string]any{"stepId": runID + "-" + run.StepKey, "version": 2})
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.ErrorContains(t, err, "initial step execution")
	write("updateWorkRun", map[string]any{"runId": runID, "templateFingerprint": "different-generation"})
	_, err = b.ReadCompletedWorkOutcomeFormat(runtime, input)
	require.ErrorContains(t, err, "retained consumed attempt")
}
