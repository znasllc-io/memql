package work

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestReplyReviewHelperPauseRetainsOperationalSourceForGuidanceAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
		peer.eng.SetScopedWorkflowRunner(func(ctx context.Context, name string, args map[string]any, ops map[string]memql.WorkflowOperation) (any, error) {
			return workflowhost.Run(ctx, name, args, workflowhost.Options{AmbientEngine: peer.eng, Operations: ops})
		})
	}
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "finish", 0, 1, nil, "", map[string]any{"status": "running"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"finish"}, "spent": map[string]any{"modelCalls": 3, "retries": 2}})
	run := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "finish", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(actorCtx(a.owner), run)
	source := []common.ChatMessage{{Role: "system", Content: "Operational instructions"}, {Role: "user", Content: "Save once"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "saveFile", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "saved", Content: `{"saved":true}`}, {Role: "system", Name: "work_recovery_checkpoint", Content: `{"version":1,"discoveryRounds":4,"repeatedRounds":3,"reviewActive":true,"stalledAfterReview":2,"guidanceIds":["previous-guidance"]}`}}
	require.NoError(t, a.eng.SaveWorkContinuation(ctx, source))
	before, err := a.eng.ReadWorkReplyReview(ctx)
	require.NoError(t, err)
	helper := []common.ChatMessage{{Role: "system", Content: "Review helper contract"}, {Role: "user", Content: "Compacted helper preview; original source remains archived"}}
	retry, err := a.eng.RecoverWorkModelCall(memql.ContextWithWorkReplyReviewSource(ctx, source), "context checkpoint cannot fit the active request, tool contracts and pending exchange; history retained", 0, 0, helper)
	require.False(t, retry)
	var wait *workstate.HumanWait
	require.ErrorAs(t, err, &wait)
	_, err = b.i.handleDecideApproval(actorCtx(a.owner), map[string]any{"approvalId": wait.ApprovalID, "decision": "answered", "answer": map[string]any{"feedback": "Finish the remaining journal entry; reuse the saved file."}}, 0)
	require.NoError(t, err)
	prepared, err := b.eng.PrepareWorkContinuation(ctx, memql.CheckpointResumeRequest{RunID: runID, StepKey: run.StepKey, ApprovalID: wait.ApprovalID})
	require.NoError(t, err)
	restored, err := b.eng.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, source, restored, "helper context must not replace completed effects or recovery watermarks")
	after, err := b.eng.ReadWorkReplyReview(prepared)
	require.NoError(t, err)
	require.Equal(t, before.Binding.Generation, after.Binding.Generation)
	require.NotEqual(t, before.Binding.Guidance, after.Binding.Guidance)
	guidance, err := b.eng.ReadWorkGuidance(prepared)
	require.NoError(t, err)
	require.Contains(t, memql.WorkGuidanceText(guidance), "Finish the remaining journal entry")
	require.Equal(t, float64(3), rowMap(a.runRow(t, runID), "spent")["modelCalls"], "recovery preserves cumulative usage")
}

func TestContextCapacityDecisionPreservesCheckpointGenerationAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, answer := range []map[string]any{{"value": "retry"}, {"feedback": "Continue the saved result after repair."}} {
		a, b := freshActsDBPair(t, first, second)
		b.owner = a.owner
		runID := a.openRun(t, nil)
		person := actorCtx(a.owner)
		override := map[string]any{"model": "gpt-6-astra", "effort": "xhigh", "wholePrompt": true, "prompt": "Complete only the existing draft", "requestedBy": a.owner}
		a.writeVersion(t, runID, runID, "reason", 0, 2, nil, "", map[string]any{"status": "waiting", "override": override})
		rerun := map[string]any{"requestId": "existing-generation", "stepKey": "reason", "reason": rerunReasonRerun, "versions": map[string]any{"reason": 2}, "override": override}
		a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"reason"}, "rerun": rerun, "spent": map[string]any{"modelCalls": 3, "retries": 2}})
		rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive, RerunRequestId: "existing-generation", Override: common.StepOverrideFromRecord(override)}
		ctx := common.ContextWithRun(person, rc)
		saved := []common.ChatMessage{{Role: "system", Content: "Existing authority"}, {Role: "user", Content: "Existing draft"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read", Name: "read", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "read", Content: "Completed evidence; do not replay"}}
		require.NoError(t, a.eng.SaveWorkContinuation(ctx, saved))
		checkpoint := a.query(t, a.owner, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": "reason"}))[0]
		approval, err := a.i.PauseWorkForModelCall(ctx, "context checkpoint cannot fit the active request, tool contracts and pending exchange; history retained", rowString(checkpoint, "id"), "Continue after the capacity repair?")
		require.NoError(t, err)
		before := a.runRow(t, runID)
		_, err = b.i.handleDecideApproval(person, map[string]any{"approvalId": approval, "decision": "answered", "answer": answer}, 0)
		require.NoError(t, err)
		after := a.runRow(t, runID)
		for _, field := range []string{"rerun", "head", "spent", "stepOrder", "staleSteps"} {
			require.Equal(t, before[field], after[field], field)
		}
		require.Equal(t, "running", after["status"])
		prepared, err := b.eng.PrepareWorkContinuation(ctx, memql.CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: approval, RerunRequestID: "existing-generation"})
		require.NoError(t, err)
		restored, err := b.eng.RestoreWorkContinuation(prepared, nil)
		require.NoError(t, err)
		require.Equal(t, saved, restored)
	}
}
