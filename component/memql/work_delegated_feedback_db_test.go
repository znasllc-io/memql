package memql

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestDelegatedFeedbackPersistsExactEffectsAndWaitAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := WithActingAgentId(contextTestRun(t, a), "coding-agent")
	run, _ := common.RunFromContext(ctx)
	sessionID, approvalID := id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		runMutation(t, auth.ContextWithInternalOrigin(ctx), a, name, args)
	}
	write("createAppSession", map[string]any{"sessionId": sessionID, "workerId": "test-worker", "app": "codex", "kind": "run", "runId": run.RunId, "stepId": run.StepKey, "workspace": "/work", "prompt": "Original request", "startedAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)})
	initial := []common.ChatMessage{{Role: "system", Content: "Current instructions"}, {Role: "user", Content: "Original request"}}
	// More than one query page, with another session interleaved. Every
	// completed effect must survive exactly and in order, without that session.
	for n := range 20 {
		require.NoError(t, a.RecordDelegatedToolSource(ContextWithDelegatedSession(ctx, sessionID), "writeFile", map[string]any{"path": fmt.Sprintf("file-%02d", n)}, fmt.Sprintf("Written once %02d; NUL \x00; UTF-8 🌿", n)))
	}
	require.NoError(t, a.RecordDelegatedToolSource(ContextWithDelegatedSession(ctx, "other-session"), "writeFile", nil, "Unrelated effect"))
	args := map[string]any{"question": "Please sign in in the separate browser tab", "runId": run.RunId}
	require.NoError(t, b.PrepareDelegatedFeedback(ctx, sessionID, initial, "requestUserFeedback", args))
	restored, err := a.RestoreWorkContinuation(ctx, nil)
	require.NoError(t, err)
	require.Len(t, restored, 44)
	require.Equal(t, initial, restored[:2])
	for n := range 20 {
		call, result := restored[2+n*2], restored[3+n*2]
		require.Equal(t, call.ToolCalls[0].ID, result.ToolCallId)
		require.Equal(t, fmt.Sprintf("Written once %02d; NUL \x00; UTF-8 🌿", n), result.Content)
	}
	require.Equal(t, "requestUserFeedback", restored[42].ToolCalls[0].Name)
	require.Equal(t, common.UnexecutedWorkToolResult, restored[43].Content)
	require.NoError(t, b.DelegatedWorkHumanWait(ctx, sessionID), "no model-authored text can create a wait")
	write("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": run.RunId, "stepKey": run.StepKey, "kind": "feedback", "artifactHash": "question", "question": "Please sign in", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("updateWorkRun", map[string]any{"runId": run.RunId, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}})
	var wait *work.HumanWait
	require.ErrorAs(t, b.DelegatedWorkHumanWait(ctx, sessionID), &wait)
	require.Equal(t, approvalID, BareShortId(wait.ApprovalID))
	write("decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "decidedBy": run.OwnerUserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "Signed in"}})
	write("updateWorkRun", map[string]any{"runId": run.RunId, "status": "running", "humanResumeId": approvalID, "waitingOn": map[string]any{}})
	require.ErrorAs(t, b.DelegatedWorkHumanWait(ctx, sessionID), &wait, "an early answer must still end the old invocation")
	prepared, err := b.PrepareWorkContinuation(ctx, CheckpointResumeRequest{RunID: run.RunId, StepKey: run.StepKey, ApprovalID: approvalID})
	require.NoError(t, err)
	require.NoError(t, b.DelegatedWorkHumanWait(prepared, sessionID), "the resumed invocation must not pause on its own answered question")
	again, err := b.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, restored, again)
	wrong := run
	wrong.GoalId = "another-goal"
	require.Error(t, b.DelegatedWorkHumanWait(common.ContextWithRun(ctx, wrong), sessionID))
	wrong = run
	wrong.RerunRequestId = "superseded"
	require.Error(t, b.DelegatedWorkHumanWait(common.ContextWithRun(ctx, wrong), sessionID))
	require.Error(t, b.PrepareDelegatedFeedback(ctx, sessionID, nil, "requestUserFeedback", args))
	write("updateWorkRun", map[string]any{"runId": run.RunId, "cancelRequested": true})
	require.Error(t, b.DelegatedWorkHumanWait(ctx, sessionID))
}
