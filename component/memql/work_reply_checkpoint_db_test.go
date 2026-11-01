package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

func TestReplyCheckpointPersistsTransitionsAcrossReplicas(t *testing.T) {
	a, b, ctx, _, runID, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Save the requested evidence", "origin": "user"})
	runtime := guidanceRunContext(ctx, runID)
	initial, err := b.ReadWorkReplyReview(runtime)
	require.NoError(t, err)
	require.Empty(t, initial.Pending)
	for _, value := range []json.RawMessage{json.RawMessage(`{"draft":"A"}`), nil, json.RawMessage(`{"draft":"B"}`), nil} {
		require.NoError(t, a.SaveWorkReplyReview(runtime, initial.Binding, value))
		state, err := b.ReadWorkReplyReview(runtime)
		require.NoError(t, err)
		require.Equal(t, []byte(value), []byte(state.Pending), "a second retirement must supersede its newer draft")
	}
	require.NoError(t, a.SaveWorkReplyReview(runtime, initial.Binding, json.RawMessage(`{"draft":"saved"}`)))
	actor, _ := auth.AccessFromContext(ctx)
	recordApprovalGuidance(write, actor.UserId, runID, "Use the corrected evidence title")
	changed, err := b.ReadWorkReplyReview(runtime)
	require.NoError(t, err)
	require.Equal(t, initial.Binding.Generation, changed.Binding.Generation)
	require.NotEqual(t, initial.Binding.Guidance, changed.Binding.Guidance)
	require.Equal(t, initial.Binding, changed.Saved)
	require.NotEmpty(t, changed.Pending, "changed direction retains the completed result for reconciliation")
	write("updateWorkRun", map[string]any{"runId": runID, "templateFingerprint": "new-plan"})
	changed, err = b.ReadWorkReplyReview(runtime)
	require.NoError(t, err)
	require.NotEqual(t, initial.Binding.Generation, changed.Binding.Generation)
	require.Empty(t, changed.Pending, "a different plan cannot borrow an old result")
	require.Error(t, a.SaveWorkReplyReview(runtime, initial.Binding, json.RawMessage(`{}`)))
}

func TestReplyCheckpointRejectsChangedAuthorityAndCorruptState(t *testing.T) {
	a, b, ctx, _, runID, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Save the requested evidence", "origin": "user"})
	runtime := guidanceRunContext(ctx, runID)
	run, _ := common.RunFromContext(runtime)
	initial, err := a.ReadWorkReplyReview(runtime)
	require.NoError(t, err)
	foreign := common.ContextWithRun(askTestActor(), run)
	_, err = b.ReadWorkReplyReview(foreign)
	require.Error(t, err)
	cancelled, cancel := context.WithCancel(runtime)
	cancel()
	_, err = b.ReadWorkReplyReview(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	replay := run
	replay.Mode = common.RunModeReplay
	_, err = b.ReadWorkReplyReview(common.ContextWithRun(ctx, replay))
	require.Error(t, err)
	write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
	_, err = b.ReadWorkReplyReview(runtime)
	require.Error(t, err)
	require.Error(t, a.SaveWorkReplyReview(runtime, initial.Binding, json.RawMessage(`{}`)))
	write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": false, "rerun": map[string]any{"requestId": "new-request", "stepKey": run.StepKey, "override": map[string]any{"prompt": "New instructions"}}})
	_, err = b.ReadWorkReplyReview(runtime)
	require.Error(t, err)
	run.RerunRequestId = "new-request"
	_, err = b.ReadWorkReplyReview(common.ContextWithRun(ctx, run))
	require.ErrorContains(t, err, "override changed")
	run.Override = &common.StepOverride{Prompt: "New instructions"}
	// A closed goal refuses both reads and writes, even with a live run.
	write("updateWorkGoal", map[string]any{"goalId": "fixture-goal", "status": "closed"})
	_, err = b.ReadWorkReplyReview(common.ContextWithRun(ctx, run))
	require.Error(t, err)
	write("updateWorkGoal", map[string]any{"goalId": "fixture-goal", "status": "open"})
	write("updateWorkRun", map[string]any{"runId": runID, "rerun": map[string]any{}})
	write("createWorkObservation", map[string]any{"observationId": "corrupt-reply-stage", "runId": runID, "stepKey": run.StepKey, "kind": "note", "content": "Synthetic corrupt state", "data": map[string]any{"replyReviewStage": map[string]any{"version": 1}}})
	_, err = b.ReadWorkReplyReview(runtime)
	require.ErrorContains(t, err, "unreadable reply review checkpoint")
	write("updateWorkRun", map[string]any{"runId": runID, "status": "succeeded"})
	_, err = b.ReadWorkReplyReview(runtime)
	require.ErrorContains(t, err, "stopped", "settled execution cannot use a retained draft as a new delivery request")
}
