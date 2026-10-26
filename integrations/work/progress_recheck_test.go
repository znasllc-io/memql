package work

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	workstate "github.com/znasllc-io/memql/component/work"
)

func staleProgressStopFixture(t *testing.T) (*Integration, *recordingEngine, map[string]any, map[string]any, map[string]any) {
	t.Helper()
	i, eng, approval, run := guidanceFixture(t, workstate.ApprovalKindFeedback)
	i.decisionGate = func(context.Context, ...string) (func(), error) { return func() {}, nil }
	request := workstate.ProgressFeedback(actRunId, "draft", workstate.CeilingBreach{Ceiling: workstate.CeilingModelCalls, Limit: "96 calls", Actual: "317 made"}, testNow, DefaultApprovalTTL)
	workstate.StopProgressFeedback(&request)
	request.Subject["resumeKind"], request.Subject["resumeReason"] = waitKindReplan, "Keep the saved candidate; hold its merge."
	approval["stepKey"] = "draft"
	approval["subject"], approval["artifactHash"], approval["question"], approval["options"] = request.Subject, artifactHashOf(request.Subject), request.Question, request.Options
	run["classification"] = map[string]any{"workload": "project", "progressContinuation": map[string]any{"extensions": 8, "policyVersion": 2, "receipts": []string{"credited"}, "receiptAfter": rfc(testNow)}}
	run["spent"] = map[string]any{"modelCalls": 317, "tokens": 1591440, "wallClockMs": 64481783, "retries": 2}
	goal := map[string]any{"id": actGoalId, "ownerUserId": actOwner, "status": "open", "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000, "maxRetries": 2}}
	eng.reply("workGoalForOwner", goal)
	return i, eng, approval, run, goal
}

func TestStaleProgressStopRechecksExistingWindowWithoutRenewal(t *testing.T) {
	testProgressStopRechecksExistingWindowWithoutRenewal(t, workstate.CeilingModelCalls, false)
}

func TestUnevaluatedStopRechecksExistingWindowWithoutRenewal(t *testing.T) {
	testProgressStopRechecksExistingWindowWithoutRenewal(t, workstate.CeilingUnevaluated, false)
}

func TestUnevaluatedStopRechecksInitialWindowWithoutRenewal(t *testing.T) {
	testProgressStopRechecksExistingWindowWithoutRenewal(t, workstate.CeilingUnevaluated, true)
}

func testProgressStopRechecksExistingWindowWithoutRenewal(t *testing.T, ceiling string, initial bool) {
	i, eng, approval, run, _ := staleProgressStopFixture(t)
	rowMap(approval, "subject")["ceiling"] = ceiling
	approval["artifactHash"] = artifactHashOf(rowMap(approval, "subject"))
	if initial {
		delete(run, "classification")
		run["spent"] = map[string]any{"modelCalls": 1, "tokens": 20, "wallClockMs": 1000}
	}
	ctx := callerContext(actOwner)
	require.True(t, i.ApprovalGuidanceAvailable(ctx, approval))
	nodes, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "Continue the unfinished publication."}}, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Len(t, eng.callsTo("updateWorkRun"), 1)
	update := eng.callTo(t, "updateWorkRun").Args(t)
	for _, field := range []string{"spent", "classification", "rerun", "head", "staleSteps"} {
		require.NotContains(t, update, field)
	}
	wait := rowMap(update, "waitingOn")
	require.Equal(t, waitKindReplan, rowString(wait, "kind"))
	require.Contains(t, rowString(wait, "reason"), "Keep the saved candidate; hold its merge.")
	require.Contains(t, rowString(wait, "reason"), "Continue the unfinished publication.")
	require.Empty(t, eng.callsTo("updateWorkGoal"))
}

func TestStaleProgressStopRecheckRefusesWithoutCurrentAllowance(t *testing.T) {
	testProgressStopRecheckRefusesWithoutCurrentAllowance(t, workstate.CeilingModelCalls)
}

func TestUnevaluatedStopRecheckRefusesWithoutCurrentAllowance(t *testing.T) {
	testProgressStopRecheckRefusesWithoutCurrentAllowance(t, workstate.CeilingUnevaluated)
}

func testProgressStopRecheckRefusesWithoutCurrentAllowance(t *testing.T, ceiling string) {
	for _, scenario := range []string{"tokens", "cost", "call window", "time window", "closed goal", "missing goal", "missing spend", "malformed spend", "missing cost", "null calls", "null tokens", "null time", "null cost", "infinite cost", "negative calls", "missing window", "missing intent", "unknown resume", "not progress", "wrong ceiling", "review", "cancelled", "moved", "foreign owner"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, approval, run, goal := staleProgressStopFixture(t)
			rowMap(approval, "subject")["ceiling"] = ceiling
			switch scenario {
			case "tokens":
				rowMap(goal, "ceilings")["tokenBudget"] = 100
			case "cost":
				rowMap(goal, "ceilings")["costCeiling"] = 1
				rowMap(run, "spent")["cost"] = 1
			case "call window":
				rowMap(run, "spent")["modelCalls"] = 864
			case "time window":
				rowMap(run, "spent")["wallClockMs"] = 64800000
			case "closed goal":
				goal["status"] = "closed"
			case "missing goal":
				eng.reply("workGoalForOwner")
			case "missing spend":
				delete(run, "spent")
			case "malformed spend":
				rowMap(run, "spent")["tokens"] = "unknown"
			case "missing cost":
				rowMap(goal, "ceilings")["costCeiling"] = 1
			case "null calls":
				rowMap(run, "spent")["modelCalls"] = nil
			case "null tokens":
				rowMap(run, "spent")["tokens"] = nil
			case "null time":
				rowMap(run, "spent")["wallClockMs"] = nil
			case "null cost":
				rowMap(goal, "ceilings")["costCeiling"] = 1
				rowMap(run, "spent")["cost"] = nil
			case "infinite cost":
				rowMap(run, "spent")["cost"] = math.Inf(1)
			case "negative calls":
				rowMap(run, "spent")["modelCalls"] = -1
			case "missing window":
				delete(run, "classification")
			case "missing intent":
				delete(rowMap(approval, "subject"), "resumeReason")
			case "unknown resume":
				rowMap(approval, "subject")["resumeKind"] = "unknown"
			case "not progress":
				delete(rowMap(approval, "subject"), "progressLimit")
			case "wrong ceiling":
				rowMap(approval, "subject")["ceiling"] = workstate.CeilingTokens
			case "review":
				rowMap(approval, "subject")["reviewType"] = "result"
			case "cancelled":
				run["cancelRequested"] = true
			case "moved":
				rowMap(run, "waitingOn")["subject"] = "other-approval"
			case "foreign owner":
				approval["ownerUserId"] = "other-owner"
			}
			approval["artifactHash"] = artifactHashOf(rowMap(approval, "subject"))
			require.False(t, i.ApprovalGuidanceAvailable(callerContext(actOwner), approval))
			_, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "Continue."}}, 0)
			require.Error(t, err)
			require.Empty(t, mutationsIn(eng))
		})
	}
}
