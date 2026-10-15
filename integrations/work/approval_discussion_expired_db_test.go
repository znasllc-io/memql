package work

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Reuse the discussion test's two independent engines: opening happens on the
// other replica, with no model call or new database/engine fixture.
func testExpiredScopeDiscussionResumesCheckpointWithoutGrant(t *testing.T, a, b actsDB) {
	person := actorCtx(a.owner)
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	a.writeVersion(t, runID, runID, "inspect", 0, 1, nil, "completed inspection", nil)
	a.writeVersion(t, runID, runID, "reason", 1, 1, nil, "", map[string]any{"status": "waiting"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"inspect", "reason"}, "spent": map[string]any{"modelCalls": 3}, "rerun": map[string]any{"requestId": "existing-attempt"}})
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive, RerunRequestId: "existing-attempt"}
	runCtx := common.ContextWithRun(person, rc)
	saved := []common.ChatMessage{{Role: "system", Content: "Existing authority"}, {Role: "user", Content: "Inspect the selected repository"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read-once", Name: "readSource", Arguments: `{}`}}},
		{Role: "tool", ToolCallId: "read-once", Name: "readSource", Content: "Completed inspection"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "pending", Name: "requestComputerUseScope", Arguments: `{}`}}},
		{Role: "tool", ToolCallId: "pending", Name: "requestComputerUseScope", Content: common.UnexecutedWorkToolResult}}
	require.NoError(t, a.eng.SaveWorkContinuation(runCtx, saved))
	fixture, request := computerPermissionFixture()
	subject := rowMap(fixture, "subject")
	approvalID := newRowId(approvalConcept)
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": rc.StepKey, "kind": "scopeElevation", "subject": subject, "artifactHash": artifactHashOf(subject), "requestedAt": rfc(time.Now().Add(-2 * time.Hour)), "expiresAt": rfc(time.Now().Add(-time.Hour))})
	waiting := map[string]any{"kind": "approval", "subject": approvalID}
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "waitingOn": waiting})
	before := a.runRow(t, runID)
	readApproval := func() map[string]any {
		t.Helper()
		rows, err := b.i.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
		require.NoError(t, err)
		require.Len(t, rows, 1)
		return rows[0]
	}
	original := readApproval()
	opened := discussionExecute(t, b.eng, person, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
	require.Equal(t, "expired", rowMap(opened, "approval")["status"])
	require.Equal(t, true, opened["canSendGuidance"], "expired native requests still accept a correction")
	require.Equal(t, false, opened["canAskAdvisor"])
	require.Equal(t, original, readApproval(), "opening must not extend or decide the expired request")
	unchanged := a.runRow(t, runID)
	for _, field := range []string{"status", "waitingOn", "spent", "rerun", "stepOrder", "head"} {
		require.Equal(t, before[field], unchanged[field], field)
	}
	// Expiry is still an authority boundary, and opening Ask must not admit an
	// analysis child or let another person supply the answer.
	for _, mode := range []string{"once", "always"} {
		result, err := b.i.handleDecideApproval(person, map[string]any{"approvalId": approvalID, "decision": "approved", "answer": map[string]any{"permissionMode": mode}}, 0)
		require.NoError(t, err)
		require.Equal(t, "expired", decodeReply(t, result)["permissionState"])
		require.Equal(t, original, readApproval())
		require.Equal(t, "waiting", rowString(a.runRow(t, runID), "status"))
	}
	_, err := b.eng.Execute(person, "builtin "+call("askApprovalMessage", map[string]any{"approvalId": approvalID, "conversationId": opened["conversationId"], "requestId": "expired-advisor", "prompt": "Explain this request", "advisor": true}))
	require.ErrorContains(t, err, "no longer pending")
	_, err = b.eng.Execute(actorCtx("different-person"), "builtin "+call("askApprovalDiscussion", map[string]any{"approvalId": approvalID}))
	require.Error(t, err)
	guidance := map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": "Reconsider the unfinished work and request fresh access if needed."}}
	_, err = b.i.handleDecideApproval(actorCtx("different-person"), guidance, 0)
	require.Error(t, err)
	// A replica with a previously opened discussion must see cancellation and
	// replacement immediately, both in the projection and at answer admission.
	for _, change := range []map[string]any{{"cancelRequested": true}, {"waitingOn": map[string]any{"kind": "approval", "subject": "replacement"}}} {
		change["runId"] = runID
		a.write(t, "updateWorkRun", change)
		blocked := discussionExecute(t, b.eng, person, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
		require.Equal(t, false, blocked["canSendGuidance"])
		_, err = b.i.handleDecideApproval(person, guidance, 0)
		require.Error(t, err)
		require.Equal(t, original, readApproval())
		a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": false, "waitingOn": waiting})
	}
	nodes, err := b.i.handleDecideApproval(person, guidance, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	resumed, err := a.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
	require.NoError(t, err)
	require.Equal(t, "running", resumed["status"])
	require.Equal(t, memql.BareShortId(approvalID), resumed["humanResumeId"])
	for _, field := range []string{"spent", "rerun", "stepOrder", "head"} {
		require.Equal(t, before[field], resumed[field], field)
	}
	require.Empty(t, resumed["staleSteps"])
	answered := readApproval()
	require.Equal(t, "answered", answered["decision"])
	require.Equal(t, guidance["answer"], answered["answer"])
	prepared, err := a.eng.PrepareWorkContinuation(runCtx, memql.CheckpointResumeRequest{RunID: runID, StepKey: rc.StepKey, ApprovalID: approvalID, RerunRequestID: rc.RerunRequestId})
	require.NoError(t, err)
	restored, err := a.eng.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, saved, restored, "continuation must retain completed tools and the unexecuted request")
	request.OwnerID, request.RunID = a.owner, runID
	grant, err := a.i.Find(person, request)
	require.NoError(t, err)
	require.Nil(t, grant, "guidance must never become computer access")
	reopened := discussionExecute(t, a.eng, person, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
	require.Equal(t, "answered", rowMap(reopened, "approval")["status"])
	require.Equal(t, false, reopened["canSendGuidance"])
	require.Equal(t, false, reopened["canAskAdvisor"])
}
