package work

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func continuationFixture(t *testing.T) (*Integration, *recordingEngine, map[string]any, map[string]any) {
	t.Helper()
	i, e, steps := newActsIntegration(t)
	addPristineRun(steps, actRunId, "fetch", "draft")
	run := actRunRow(runStatusSucceeded, "fetch", "draft")
	sealed := installedReplanRun(t)
	run["automationName"], run["templateFingerprint"] = sealed["automationName"], sealed["templateFingerprint"]
	delete(run, "templateConstructId")
	run["createdAt"] = rfc(testNow)
	run["spent"] = map[string]any{"modelCalls": 73, "tokens": 4567}
	run["classification"] = map[string]any{"progressContinuation": map[string]any{"extensions": 2}}
	e.reply("workRunForOwner", run)
	e.reply("workRunsForGoal", run)
	args := map[string]any{"runId": actRunId, "runVersion": rfc(testNow), "requestId": "turn-1", "feedback": "Finish the missing delivery using the saved draft."}
	return i, e, run, args
}

func TestContinueFinishedResultPreservesRunAndRequestsNativePlan(t *testing.T) {
	i, e, _, args := continuationFixture(t)
	nodes, err := i.handleContinueRun(callerContext(actOwner), args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["continuationRequested"])
	write := e.callTo(t, "updateWorkRun").Args(t)
	require.Equal(t, actRunId, write["runId"])
	require.Equal(t, runStatusWaiting, write["status"])
	require.Equal(t, waitKindReplan, rowString(rowMap(write, "waitingOn"), "kind"))
	require.Equal(t, "draft", rowString(rowMap(write, "waitingOn"), "subject"))
	require.Equal(t, args["feedback"], rowMap(write, "feedbackContinuation")["feedback"])
	for _, field := range []string{"spent", "head", "routing", "continuation", "cancelRequested", "cancelledBy"} {
		_, present := write[field]
		require.False(t, present, "continuation must preserve %s", field)
	}
	require.Empty(t, e.callsTo("updateWorkGoal"))
	require.Empty(t, e.callsTo("createWorkRun"))
	require.Empty(t, e.callsTo("updateWorkStep"))
}

func TestContinueFinishedResultRefusesStaleOrUnownedWork(t *testing.T) {
	for _, name := range []string{"accepted", "closed", "stale", "cancelled", "child", "foreign", "model", "running", "newer", "other active", "replay", "no template", "empty feedback", "bad version"} {
		t.Run(name, func(t *testing.T) {
			i, e, run, args := continuationFixture(t)
			ctx := callerContext(actOwner)
			switch name {
			case "accepted":
				e.reply("workGoalForOwner", map[string]any{"ownerUserId": actOwner, "status": "open", "acceptance": map[string]any{"runId": actRunId}})
			case "closed":
				e.reply("workGoalForOwner", map[string]any{"ownerUserId": actOwner, "status": "closed"})
			case "stale":
				args["runVersion"] = rfc(testNow.Add(-time.Second))
			case "cancelled":
				run["cancelRequested"] = true
			case "child":
				run["parentRunId"] = "parent"
			case "foreign":
				run["ownerUserId"] = "someone-else"
			case "model":
				ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "other", GoalId: actGoalId, OwnerUserId: actOwner})
			case "running":
				run["status"] = runStatusRunning
			case "newer":
				e.reply("workContinuationConflictsForOwnerGoal", map[string]any{"id": "newer", "status": runStatusSucceeded, "createdAt": rfc(testNow.Add(time.Second))})
			case "other active":
				e.reply("workContinuationConflictsForOwnerGoal", map[string]any{"id": "other", "status": runStatusRunning, "createdAt": rfc(testNow.Add(-time.Second))})
			case "replay":
				run["mode"] = "replay"
			case "no template":
				run["automationName"] = ""
			case "empty feedback":
				args["feedback"] = "  "
			case "bad version":
				args["runVersion"] = "invalid"
			}
			_, err := i.handleContinueRun(ctx, args, 0)
			require.Error(t, err)
			require.Empty(t, mutationsIn(e))
		})
	}
}

func TestContinueRechecksAcceptanceInsideLifecycleGate(t *testing.T) {
	i, e, _, args := continuationFixture(t)
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		require.Equal(t, []string{"goal-lifecycle-" + memql.BareShortId(actGoalId), "workload:" + memql.BareShortId(actRunId), "provider-admission:" + memql.BareShortId(actRunId)}, keys)
		e.reply("workGoalForOwner", map[string]any{"ownerUserId": actOwner, "status": "closed"})
		return func() {}, nil
	}
	_, err := i.handleContinueRun(callerContext(actOwner), args, 0)
	require.Error(t, err)
	require.Empty(t, mutationsIn(e))
}

func TestContinueRetryUsesDurableReceiptWithoutDispatchingAgain(t *testing.T) {
	i, e, run, args := continuationFixture(t)
	run["status"] = runStatusRunning
	run["createdAt"] = rfc(testNow.Add(time.Second))
	receipt := map[string]any{"schemaVersion": 1, "id": memql.WorkResultFeedbackID(actRunId, rowString(args, "requestId")), "requestId": args["requestId"], "runVersion": args["runVersion"], "feedback": args["feedback"], "requestedBy": actOwner, "requestedAt": rfc(testNow), "stepKey": "draft", "accepted": true}
	run["feedbackContinuation"] = receipt
	e.reply("workResultFeedbackForOwnerRequest", map[string]any{"id": receipt["id"], "runId": actRunId, "ownerUserId": actOwner, "createdAt": rfc(testNow), "data": map[string]any{"resultFeedback": receipt}})
	_, err := i.handleContinueRun(callerContext(actOwner), args, 0)
	require.NoError(t, err)
	require.Empty(t, mutationsIn(e))
	args["feedback"] = "Different work"
	_, err = i.handleContinueRun(callerContext(actOwner), args, 0)
	require.Error(t, err)
	require.Empty(t, mutationsIn(e))
}

func TestContinueFeedbackWindowUsesProviderSpendAndKeepsHardLimits(t *testing.T) {
	for _, limit := range []string{"none", "tokens", "cost", "unknown coverage", "bad accounting"} {
		t.Run(limit, func(t *testing.T) {
			i, e, run, args := continuationFixture(t)
			run["startedAt"] = rfc(testNow.Add(-time.Hour))
			goal := map[string]any{"id": actGoalId, "ownerUserId": actOwner, "status": "open", "ceilings": map[string]any{"maxModelCalls": 96}}
			ledger := map[string]any{"schemaVersion": 1, "legacyAnsweredCalls": 0, "admittedCalls": 1300, "estimatedTokens": 10000, "estimatedCost": 5, "coverage": "complete-from-run-creation"}
			switch limit {
			case "tokens":
				rowMap(goal, "ceilings")["tokenBudget"] = 9000
			case "cost":
				rowMap(goal, "ceilings")["costCeiling"] = 4
			case "unknown coverage":
				ledger["coverage"] = "legacy-attempts-unknown"
				rowMap(goal, "ceilings")["costCeiling"] = 50
			case "bad accounting":
				delete(ledger, "admittedCalls")
			}
			e.reply("workGoalForOwner", goal)
			e.reply("workProviderAdmissionForOwnerRun", map[string]any{"ownerUserId": actOwner, "runId": actRunId, "createdAt": rfc(testNow), "data": map[string]any{"providerAdmission": ledger}})
			_, err := i.handleContinueRun(callerContext(actOwner), args, 0)
			if limit != "none" {
				require.Error(t, err)
				require.Empty(t, mutationsIn(e))
				return
			}
			require.NoError(t, err)
			write := e.callTo(t, "updateWorkRun").Args(t)
			run["classification"] = write["classification"]
			declared, err := ceilingsOf(goal)
			require.NoError(t, err)
			limits, err := progressCeilings(goal, run, declared)
			require.NoError(t, err)
			require.Nil(t, workstate.CheckCeilings(limits, workstate.Spent{ModelCalls: 1301, Tokens: 10000, Cost: 5, WallClockMs: time.Hour.Milliseconds()}, 0))
			require.Empty(t, e.callsTo("updateWorkGoal"))
			require.NotContains(t, write, "spent")
		})
	}
}
