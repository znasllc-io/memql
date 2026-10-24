package work

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestProgressStopRecheckDBAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	person := actorCtx(a.owner)
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "draft", Mode: common.RunModeLive}
	guard := NewRunCeilings(a.eng, testLogger())
	require.NoError(t, guard.AdmitProvider(person, rc, memql.ProviderAdmission{HardCallLimit: 1}))
	providerBefore := a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Len(t, providerBefore, 1)
	a.writeVersion(t, runID, runID, "draft", 0, 1, nil, "", map[string]any{"status": "waiting"})
	limits := map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000}
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": limits})
	request := workstate.ProgressFeedback(runID, "draft", workstate.CeilingBreach{Ceiling: workstate.CeilingModelCalls, Limit: "96 calls", Actual: "317 made"}, time.Now(), time.Hour)
	workstate.StopProgressFeedback(&request)
	request.Subject["resumeKind"], request.Subject["resumeReason"] = waitKindReplan, "Reuse the finished candidate and hold its merge."
	approvalID := newRowId(approvalConcept)
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "draft", "kind": "feedback", "subject": request.Subject, "artifactHash": artifactHashOf(request.Subject), "question": request.Question, "options": request.Options, "requestedAt": rfc(time.Now())})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusWaiting, "stepOrder": []string{"draft"}, "spent": map[string]any{"modelCalls": 317, "tokens": 1591440, "wallClockMs": 64481783, "retries": 2}, "classification": map[string]any{"workload": "project", "progressContinuation": map[string]any{"policyVersion": 2, "extensions": 8, "receipts": []string{"credited"}, "receiptAfter": rfc(time.Now())}}, "waitingOn": map[string]any{"kind": "approval", "subject": approvalID, "since": rfc(time.Now())}})
	before := a.runRow(t, runID)
	state, err := b.i.handleApprovalDiscussionState(person, map[string]any{"approvalId": approvalID}, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, state)["canSendGuidance"])
	// Save the real answer, then lose the run release. A new replica must
	// recheck the current allowance before recovering that exact answer.
	wrapper := &budgetResumeFailureEngine{Engine: a.eng}
	wrapper.fail.Store(true)
	a.i.engine = wrapper
	args := map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": "Continue only the unfinished publication and cleanup."}}
	nodes, err := a.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.Contains(t, decodeReply(t, nodes)["resumeError"], "synthetic run release failure")
	accepted := b.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))[0]
	require.Equal(t, "answered", rowString(accepted, "decision"))
	require.Equal(t, artifactHashOf(request.Subject), artifactHashOf(rowMap(accepted, "subject")))
	limits["tokenBudget"] = 10
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": limits})
	nodes, err = b.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.NotEmpty(t, decodeReply(t, nodes)["resumeError"])
	require.Equal(t, before, a.runRow(t, runID))
	delete(limits, "tokenBudget")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": limits})
	nodes, err = b.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	after := a.runRow(t, runID)
	for _, field := range []string{"spent", "classification", "head", "rerun", "stepOrder"} {
		require.Equal(t, before[field], after[field], field)
	}
	require.Equal(t, waitKindReplan, rowString(rowMap(after, "waitingOn"), "kind"))
	require.Contains(t, rowString(rowMap(after, "waitingOn"), "reason"), "Reuse the finished candidate and hold its merge.")
	require.Contains(t, rowString(rowMap(after, "waitingOn"), "reason"), "Continue only the unfinished publication and cleanup.")
	nodes, err = a.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Equal(t, after, a.runRow(t, runID), "exact replay must not release work twice")
	require.Equal(t, accepted, b.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))[0])
	require.Equal(t, providerBefore, b.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID})))
	// Simulate the remedy's execution handoff. Recovery itself grants no
	// provider admission, even though the current logical window has room.
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusRunning})
	err = NewRunCeilings(b.eng, testLogger()).AdmitProvider(person, rc, memql.ProviderAdmission{HardCallLimit: 1})
	var refused *memql.RunCeilingError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "providerCalls", refused.Breach.Ceiling)
}
