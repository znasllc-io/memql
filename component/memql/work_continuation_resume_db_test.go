package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestWorkContinuationRequiresTheExactOwnedAnsweredCheckpoint(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	ac, _ := auth.AccessFromContext(ctx)
	runID, goalID, approvalID := id.NewShortId(), id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Continue saved work", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "feedback", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "reason", "kind": "feedback", "artifactHash": "question", "question": "Continue?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	run := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: ac.UserId, StepKey: "reason", Mode: common.RunModeLive}
	runCtx := common.ContextWithRun(ctx, run)
	request := CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: approvalID}
	_, err := b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "an unanswered question is not a continuation")
	write("decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "decidedBy": ac.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "Continue"}})
	write("updateWorkRun", map[string]any{"runId": runID, "status": "running", "humanResumeId": approvalID})
	_, err = b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "an answered question without a checkpoint cannot repeat a turn")
	messages := []common.ChatMessage{{Role: "user", Content: "Perform work"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "writeFile", Arguments: "{}"}}}, {Role: "tool", ToolCallId: "saved", Name: "writeFile", Content: "Written once"}}
	require.NoError(t, a.SaveWorkContinuation(runCtx, messages))
	prepared, err := b.PrepareWorkContinuation(runCtx, request)
	require.NoError(t, err)
	proof, _ := common.RunFromContext(prepared)
	require.NotNil(t, proof.Continuation)
	restored, err := b.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, messages, restored)
	_, err = b.PrepareWorkContinuation(common.ContextWithRun(askTestActor(), run), request)
	require.Error(t, err, "the checkpoint grants no cross-owner access")
	changed := request
	changed.StepKey = "other"
	_, err = b.PrepareWorkContinuation(runCtx, changed)
	require.Error(t, err, "the approval cannot resume another step")
	changed = request
	changed.ApprovalID = id.NewShortId()
	_, err = b.PrepareWorkContinuation(runCtx, changed)
	require.Error(t, err)
	// A later checkpoint is a different continuation. Never fall back to it,
	// or to a fresh turn, after admission selected the original receipt.
	newer := append([]common.ChatMessage(nil), messages...)
	newer = append(newer, common.ChatMessage{Role: "user", Content: "Different continuation"})
	require.NoError(t, a.SaveWorkContinuation(runCtx, newer))
	_, err = b.RestoreWorkContinuation(prepared, nil)
	require.Error(t, err, "changed checkpoint accepted")
	write("updateWorkRun", map[string]any{"runId": runID, "status": "cancelled"})
	_, err = b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "cancelled work resumed")
}
