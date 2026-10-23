package work

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// A session approval resumes the same native suspension, including after a
// prior explicit rerun. A new request ID would invalidate its saved checkpoint.
func TestScopeCheckpointApprovalPreservesExecutionAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, mode := range []string{"desktop", "file_write", "http", "changed-request", "rejected"} {
		for _, request := range []string{"", "existing-attempt"} {
			t.Run(mode+"/"+request, func(t *testing.T) {
				a, b := freshActsDBPair(t, first, second)
				b.owner = a.owner
				runID := a.openRun(t, nil)
				person := actorCtx(a.owner)
				a.writeVersion(t, runID, runID, "inspect", 0, 1, nil, "completed inspection", nil)
				a.writeVersion(t, runID, runID, "reason", 1, 2, nil, "", map[string]any{"status": "waiting"})
				rerun := map[string]any{}
				if request != "" {
					rerun = map[string]any{"requestId": request, "stepKey": "reason", "reason": rerunReasonRerun, "versions": map[string]any{"reason": 2}}
				}
				a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"inspect", "reason"}, "rerun": rerun, "spent": map[string]any{"modelCalls": 3, "retries": 2}})
				rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive, RerunRequestId: request}
				ctx := common.ContextWithRun(person, rc)
				session := mode
				if mode == "changed-request" || mode == "rejected" {
					session = "desktop"
				}
				args := map[string]any{"sessionKind": session, "requestedScope": "full", "intent": "verify", "summary": "Verify the synthetic result"}
				raw, err := json.Marshal(args)
				require.NoError(t, err)
				saved := []common.ChatMessage{{Role: "system", Content: "Existing authority"},
					{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read", Name: "inspect", Arguments: `{}`}}},
					{Role: "tool", Name: "inspect", ToolCallId: "read", Content: "Completed result; do not replay"},
					{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "scope", Name: "requestComputerUseScope", Arguments: string(raw)}}},
					{Role: "tool", Name: "requestComputerUseScope", ToolCallId: "scope", Content: common.UnexecutedWorkToolResult}}
				require.NoError(t, a.eng.SaveWorkContinuation(ctx, saved))
				subject, err := a.eng.BindWorkScopeCheckpoint(ctx, args, map[string]any{"agentId": "agent", "sessionKind": session, "sessionScope": "full", "intent": "verify", "summary": "Verify the synthetic result"})
				require.NoError(t, err)
				require.Contains(t, subject, "nativeScopeCheckpoint")
				if mode == "changed-request" {
					subject["nativeScopeCheckpoint"].(map[string]any)["requestHash"] = "changed"
				}
				approval, err := a.i.AskComputerScope(ctx, a.owner, runID, subject)
				require.NoError(t, err)
				before := a.runRow(t, runID)
				decision := "approved"
				if mode == "rejected" {
					decision = "rejected"
				}
				_, err = b.i.handleDecideApproval(person, map[string]any{"approvalId": approval, "decision": decision}, 0)
				require.NoError(t, err)
				after := a.runRow(t, runID)
				for _, field := range []string{"rerun", "head", "spent", "stepOrder", "staleSteps"} {
					require.Equal(t, before[field], after[field], field)
				}
				if mode == "rejected" {
					require.Equal(t, "failed", after["status"])
					return
				}
				require.Equal(t, "running", after["status"])
				prepared, err := b.eng.PrepareWorkContinuation(ctx, memql.CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: approval, RerunRequestID: request})
				if mode == "changed-request" {
					require.Error(t, err, "even a self-consistent approval hash cannot substitute a different pending call")
					return
				}
				require.NoError(t, err)
				restored, err := b.eng.RestoreWorkContinuation(prepared, nil)
				require.NoError(t, err)
				require.Equal(t, saved, restored)
				rows := a.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approval}))
				require.Equal(t, workstate.ArtifactHash(subject), rowString(rows[0], "artifactHash"))
			})
		}
	}
}
