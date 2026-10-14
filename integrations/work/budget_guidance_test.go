package work

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workstate "github.com/znasllc-io/memql/component/work"
)

func budgetGuidanceFixture(t *testing.T) (*Integration, *recordingEngine, map[string]any, map[string]any) {
	t.Helper()
	i, eng, approval, run := guidanceFixture(t, workstate.ApprovalKindBudget)
	// Production locks the approval and its goal under separate keys. The
	// general recorder's single mutex would deadlock the nested goal write.
	approvalGate := i.decisionGate
	var goalGate sync.Mutex
	i.decisionGate = func(ctx context.Context, keys ...string) (func(), error) {
		if len(keys) == 1 && strings.HasPrefix(keys[0], "goal-budget-") {
			goalGate.Lock()
			return goalGate.Unlock, nil
		}
		return approvalGate(ctx, keys...)
	}
	subject := map[string]any{"ceiling": "modelCalls", "limit": "40", "actual": "40", "resumeKind": "checkpoint", "continuationId": "saved-progress"}
	approval["subject"], approval["artifactHash"] = subject, artifactHashOf(subject)
	eng.reply("workGoalForOwner", map[string]any{"id": actGoalId, "ownerUserId": actOwner, "ceilings": map[string]any{"maxModelCalls": 40, "maxRetries": 3}})
	return i, eng, approval, run
}

func TestBudgetGuidanceSavesWithoutDecidingSpendingOrResuming(t *testing.T) {
	i, eng, approval, _ := budgetGuidanceFixture(t)
	ctx := callerContext(actOwner)
	approval["createdAt"] = testNow.Add(time.Hour)
	eng.reply("workApprovalForOwner", approval)
	state, err := i.handleApprovalDiscussionState(ctx, map[string]any{"approvalId": approval["id"]}, 0)
	require.NoError(t, err)
	require.Equal(t, "deferred", decodeReply(t, state)["guidanceMode"])
	require.Equal(t, true, decodeReply(t, state)["canSendGuidance"])
	args := map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "  Use the completed evidence.\n"}}
	nodes, err := i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	receipt := decodeReply(t, nodes)
	require.Equal(t, true, receipt["guidanceSaved"])
	require.Equal(t, false, receipt["runResumed"])
	require.Equal(t, "", receipt["decision"])
	save := argsOf(t, eng, "saveWorkApprovalGuidance")
	require.Equal(t, map[string]any{"feedback": "Use the completed evidence."}, save["answer"])
	version, err := time.Parse(time.RFC3339Nano, save["versionTime"].(string))
	require.NoError(t, err)
	require.True(t, version.After(testNow.Add(time.Hour)))
	require.Empty(t, eng.callsTo("decideWorkApproval"))
	require.Empty(t, eng.callsTo("updateWorkRun"))
	require.Empty(t, eng.callsTo("updateWorkGoal"))

	approval["answer"] = save["answer"]
	nodes, err = i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["guidanceSaved"])
	require.Len(t, eng.callsTo("saveWorkApprovalGuidance"), 1, "lost response may be retried without another saved version")
	state, err = i.handleApprovalDiscussionState(ctx, map[string]any{"approvalId": approval["id"]}, 0)
	require.NoError(t, err)
	require.Equal(t, "Use the completed evidence.", decodeReply(t, state)["savedGuidance"])
	require.Empty(t, decodeReply(t, state)["guidanceToResume"])
	args["answer"] = map[string]any{"feedback": "Use the completed evidence and finish the response."}
	nodes, err = i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.Len(t, eng.callsTo("saveWorkApprovalGuidance"), 2, "the person can replace guidance while it remains pending")
	approval["answer"] = map[string]any{"feedback": "Use the completed evidence and finish the response."}
	require.Empty(t, eng.callsTo("decideWorkApproval"))
	require.Empty(t, eng.callsTo("updateWorkRun"))
	require.Empty(t, eng.callsTo("updateWorkGoal"))

	nodes, err = i.handleDecideApproval(ctx, map[string]any{"approvalId": approval["id"], "decision": "approved", "answer": map[string]any{"newLimit": 43}}, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Equal(t, map[string]any{"feedback": "Use the completed evidence and finish the response.", "newLimit": float64(43)}, argsOf(t, eng, "decideWorkApproval")["answer"])
	update := argsOf(t, eng, "updateWorkRun")
	require.Equal(t, "running", update["status"])
	for _, key := range []string{"spent", "rerun", "staleSteps", "head", "stepOrder"} {
		require.NotContains(t, update, key)
	}
}

func TestBudgetGuidanceRefusesStaleMixedAndNonCheckpointAnswers(t *testing.T) {
	for _, scenario := range []string{"legacy budget", "legacy failure budget", "missing checkpoint", "moved", "cancelled", "other approval", "other owner", "already decided", "changed artifact", "mixed limit", "ordinary answer", "receipt lost", "write failure"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, approval, run := budgetGuidanceFixture(t)
			answer := map[string]any{"feedback": "Use the completed evidence."}
			switch scenario {
			case "legacy budget":
				delete(rowMap(approval, "subject"), "resumeKind")
			case "legacy failure budget":
				delete(rowMap(approval, "subject"), "resumeKind")
				approval["options"] = []map[string]any{{"value": workstate.FailureAnswerRetry}, {"value": workstate.FailureAnswerAbandon}}
			case "missing checkpoint":
				delete(rowMap(approval, "subject"), "continuationId")
			case "moved":
				run["status"] = "running"
			case "cancelled":
				run["cancelRequested"] = true
			case "other approval":
				rowMap(run, "waitingOn")["subject"] = "another-approval"
			case "other owner":
				approval["ownerUserId"] = "someone-else"
			case "already decided":
				approval["decision"] = "approved"
			case "changed artifact":
				approval["artifactHash"] = "changed"
			case "mixed limit":
				answer["newLimit"] = 80
			case "ordinary answer":
				answer = map[string]any{"text": "Continue"}
			case "receipt lost":
				i.computerDecisionGate = func(context.Context, string) (func(), func(context.Context) error, error) {
					return func() {}, func(context.Context) error { return errors.New("receipt lock lost") }, nil
				}
			case "write failure":
				eng.refuse("saveWorkApprovalGuidance", errors.New("storage unavailable"))
			}
			_, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": answer}, 0)
			require.Error(t, err)
			if scenario != "write failure" {
				require.Empty(t, eng.callsTo("saveWorkApprovalGuidance"))
			}
			require.Empty(t, eng.callsTo("decideWorkApproval"))
			require.Empty(t, eng.callsTo("updateWorkGoal"))
			require.Empty(t, eng.callsTo("updateWorkRun"))
		})
	}
}
