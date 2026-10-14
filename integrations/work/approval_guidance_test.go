package work

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestGuidanceRecoveryVerifiesReceiptGateBeforeResuming(t *testing.T) {
	i, eng, approval, _ := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
	approval["decision"], approval["decidedBy"], approval["answer"] = "answered", actOwner, map[string]any{"feedback": "Read the repository only."}
	eng.reply("workApprovalsForOwner")
	eng.reply("workApprovalForOwner", approval)
	verified, released := false, false
	i.computerDecisionGate = func(context.Context, string) (func(), func(context.Context) error, error) {
		return func() { released = true }, func(context.Context) error {
			verified = true
			return errors.New("receipt lock connection lost")
		}, nil
	}
	nodes, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": approval["answer"]}, 0)
	require.NoError(t, err)
	require.True(t, verified)
	require.True(t, released)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.Contains(t, decodeReply(t, nodes)["resumeError"], "receipt lock connection lost")
	require.Empty(t, eng.callsTo("updateWorkRun"))
	require.Empty(t, eng.callsTo("decideWorkApproval"))
}

func guidanceFixture(t *testing.T, kind string) (*Integration, *recordingEngine, map[string]any, map[string]any) {
	t.Helper()
	i, eng, _ := newActsIntegration(t)
	subject := map[string]any{"question": "May I inspect this directory?", "agentId": "agent-a"}
	approval := map[string]any{
		"id": "v1:work:approval:a-guidance", "runId": actRunId, "ownerUserId": actOwner, "stepKey": "draft/nested",
		"kind": kind, "subject": subject, "artifactHash": artifactHashOf(subject), "question": "May I inspect this directory?",
	}
	if kind == workstate.ApprovalKindPlanReview {
		req := workstate.FailureApproval(kind, actRunId, "draft/nested", workstate.SymptomEnvironment,
			"context checkpoint did not free space; originals were retained", "q", workstate.Evidence{}, testNow, time.Hour)
		approval["subject"], approval["artifactHash"], approval["options"] = req.Subject, req.ArtifactHash, req.Options
	}
	run := actRunRow(runStatusWaiting, "fetch", "draft", "publish")
	run["spent"] = map[string]any{"retries": 2, "modelCalls": 40, "tokens": 10000}
	run["waitingOn"] = map[string]any{"kind": "approval", "subject": approval["id"], "since": rfc(testNow.Add(-time.Minute))}
	eng.reply("workApprovalsForOwner", approval)
	eng.reply("workRunForOwner", run)
	return i, eng, approval, run
}

func TestGuidanceRecoversOnlyTheExactRecordedAnswerAfterResumeFailure(t *testing.T) {
	i, eng, approval, run := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
	ctx := callerContext(actOwner)
	args := map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "Read the repository only."}}
	eng.refuse("updateWorkRun", errors.New("connection lost after recording answer"))
	nodes, err := i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.Contains(t, decodeReply(t, nodes)["resumeError"], "connection lost")
	decision := argsOf(t, eng, "decideWorkApproval")
	approval["decision"], approval["answer"], approval["decidedBy"] = decision["decision"], decision["answer"], decision["decidedBy"]
	require.False(t, i.ApprovalGuidanceAvailable(ctx, approval))
	require.Equal(t, "Read the repository only.", i.RecoverableApprovalGuidance(ctx, approval))
	eng.reply("workApprovalsForOwner")
	eng.reply("workApprovalForOwner", approval)
	eng.refuse("updateWorkRun", nil)

	// A changed answer is not an idempotent replay and cannot authorize a plan.
	_, err = i.handleDecideApproval(ctx, map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "Read everything instead."}}, 0)
	require.Error(t, err)
	require.Len(t, eng.callsTo("updateWorkRun"), 1)
	nodes, err = i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Len(t, eng.callsTo("decideWorkApproval"), 1)
	updates := eng.callsTo("updateWorkRun")
	require.Len(t, updates, 2)
	update := updates[1].Args(t)
	require.Equal(t, waitKindReplan, rowString(rowMap(update, "waitingOn"), "kind"))
	for key, value := range update {
		if key != "runId" {
			run[key] = value
		}
	}
	require.Empty(t, i.RecoverableApprovalGuidance(ctx, approval))
	// Once handed off, the same retry acknowledges it without nudging work again.
	nodes, err = i.handleDecideApproval(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Len(t, eng.callsTo("updateWorkRun"), 2)
}

func TestRecordedGuidanceCannotResumeACancelledOrMovedRun(t *testing.T) {
	for _, scenario := range []string{"cancelled", "other approval", "other owner"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, approval, run := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
			approval["decision"], approval["decidedBy"], approval["answer"] = "answered", actOwner, map[string]any{"feedback": "Read the repository only."}
			eng.reply("workApprovalsForOwner")
			eng.reply("workApprovalForOwner", approval)
			switch scenario {
			case "cancelled":
				run["cancelRequested"] = true
			case "other approval":
				rowMap(run, "waitingOn")["subject"] = "another-request"
			case "other owner":
				approval["ownerUserId"] = "other-owner"
			}
			result, err := i.recoverGuidanceDecision(callerContext(actOwner), rowString(approval, "id"), "answered", map[string]any{"feedback": "Read the repository only."})
			require.Empty(t, i.RecoverableApprovalGuidance(callerContext(actOwner), approval))
			require.NoError(t, err)
			if result != nil {
				require.Equal(t, false, result["runResumed"])
			}
			require.Empty(t, eng.callsTo("updateWorkRun"))
			require.Empty(t, eng.callsTo("decideWorkApproval"))
		})
	}
}

func TestGuidanceReconsidersTheUnfinishedWorkWithoutApprovingAcrossReplicas(t *testing.T) {
	for _, kind := range []string{workstate.ApprovalKindFeedback, workstate.ApprovalKindScopeElevation, workstate.ApprovalKindSideEffect, workstate.ApprovalKindPlanReview} {
		t.Run(kind, func(t *testing.T) {
			i, eng, approval, run := guidanceFixture(t, kind)
			ctx := callerContext(actOwner)
			require.True(t, i.ApprovalGuidanceAvailable(ctx, approval))
			guidance := "Use the repository at /home/alice/projects/memql. Ask for access only to that directory."
			nodes, err := i.handleDecideApproval(ctx, map[string]any{
				"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "  " + guidance + "\n"},
			}, 0)
			require.NoError(t, err)
			require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
			decision := argsOf(t, eng, "decideWorkApproval")
			require.Equal(t, "answered", decision["decision"])
			require.Equal(t, map[string]any{"feedback": guidance}, rowMap(decision, "answer"))
			update := argsOf(t, eng, "updateWorkRun")
			wait := rowMap(update, "waitingOn")
			require.Equal(t, runStatusWaiting, update["status"])
			require.Equal(t, waitKindReplan, wait["kind"])
			require.Equal(t, "draft", wait["subject"])
			require.Contains(t, wait["reason"], guidance)
			require.Contains(t, wait["reason"], "no permission or budget increase was granted")
			for _, key := range []string{"spent", "rerun", "staleSteps", "head", "stepOrder", "cancelRequested"} {
				require.NotContains(t, update, key)
			}
			require.Empty(t, eng.callsTo("updateWorkGoal"))
			require.Empty(t, eng.callsTo("updateWorkStep"))
			for key, value := range update {
				if key != "runId" {
					run[key] = value
				}
			}
			claims, remedy := &pkClaims{}, &signallingRemedy{}
			a, _ := plannerReplica(t, remedy, claims, run)
			b, _ := plannerReplica(t, remedy, claims, run)
			a.HandleRunEvent(remedyEvent(run))
			b.HandleRunEvent(remedyEvent(run))
			calls := remedy.settled(t, 1)
			require.Len(t, calls, 1)
			require.Equal(t, waitKindReplan, calls[0].kind)
			require.Equal(t, "draft", calls[0].stepKey)
			require.Equal(t, wait["reason"], calls[0].reason)
			require.Equal(t, actGoalId, calls[0].run.GoalId)
		})
	}
}

func TestGuidanceRefusesInvalidOrStaleRequestsBeforeRecording(t *testing.T) {
	for _, scenario := range []string{"empty", "too long", "not text", "approved", "rejected", "mixed permission", "mixed retry", "cancelled", "moved", "other approval", "missing wait", "non executable", "unknown step", "other owner", "already decided", "artifact review", "promotion", "budget", "model context"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, approval, run := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
			ctx := callerContext(actOwner)
			decision := "answered"
			answer := map[string]any{"feedback": "Inspect just the repository."}
			switch scenario {
			case "empty":
				answer["feedback"] = " \n "
			case "too long":
				answer["feedback"] = strings.Repeat("界", 4001)
			case "not text":
				answer["feedback"] = true
			case "approved", "rejected":
				decision = scenario
			case "mixed permission":
				answer["permissionMode"] = "always"
			case "mixed retry":
				answer["value"] = "retry"
			case "cancelled":
				run["cancelRequested"] = true
			case "moved":
				run["status"] = runStatusRunning
			case "other approval":
				rowMap(run, "waitingOn")["subject"] = "another-approval"
			case "missing wait":
				delete(run, "waitingOn")
			case "non executable":
				run["automationName"] = appSessionTemplate
			case "unknown step":
				approval["stepKey"] = "nonexistent"
			case "other owner":
				approval["ownerUserId"] = "someone-else"
			case "already decided":
				approval["decision"] = "approved"
			case "artifact review":
				rowMap(approval, "subject")["reviewType"] = "draft"
			case "promotion":
				approval["kind"] = workstate.ApprovalKindProcedurePromotion
			case "budget":
				approval["kind"] = workstate.ApprovalKindBudget
			case "model context":
				ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "active-agent", OwnerUserId: actOwner})
			}
			_, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": approval["id"], "decision": decision, "answer": answer}, 0)
			require.Error(t, err)
			require.Empty(t, eng.callsTo("decideWorkApproval"))
			require.Empty(t, eng.callsTo("updateWorkRun"))
			require.Empty(t, eng.callsTo("updateWorkGoal"))
		})
	}
}

func TestGuidanceLimitCountsCharactersAndProjectionDoesNotWrite(t *testing.T) {
	i, eng, approval, _ := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
	ctx := callerContext(actOwner)
	require.True(t, i.ApprovalGuidanceAvailable(ctx, approval))
	require.Empty(t, mutationsIn(eng))
	text := strings.Repeat("界", 4000)
	got, err := i.approvalGuidance(ctx, approval, "answered", map[string]any{"feedback": text})
	require.NoError(t, err)
	require.Equal(t, text, got)
	approval["decision"] = "answered"
	require.False(t, i.ApprovalGuidanceAvailable(ctx, approval))
	require.Empty(t, mutationsIn(eng))
}

func TestGuidanceRequiresAnAdmittedContiguousJournalTail(t *testing.T) {
	for _, scenario := range []string{"missing", "gap", "old position", "nested only", "foreign latest"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, store := newActsIntegration(t)
			i.admitRow = ownerAdmits(actOwner)
			approval := map[string]any{"id": "a-guidance", "ownerUserId": actOwner, "runId": actRunId, "stepKey": "draft", "kind": workstate.ApprovalKindFeedback}
			run := actRunRow(runStatusWaiting, "fetch")
			run["waitingOn"] = map[string]any{"kind": "approval", "subject": "a-guidance"}
			eng.reply("workRunForOwner", run)
			eng.reply("workApprovalsForOwner", approval)
			addPristineRun(store, actRunId, "fetch")
			switch scenario {
			case "gap":
				addVersion(store, actRunId, "draft", 2, 1, "running", nil, nil)
			case "old position":
				addVersion(store, actRunId, "draft", 0, 1, "running", nil, nil)
			case "nested only":
				addVersion(store, actRunId, "draft/inner", 1, 1, "running", nil, nil)
			case "foreign latest":
				addVersion(store, actRunId, "draft", 1, 1, "running", nil, map[string]any{"ownerUserId": "another-owner"})
			}
			ctx := callerContext(actOwner)
			require.False(t, i.ApprovalGuidanceAvailable(ctx, approval))
			_, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": "a-guidance", "decision": "answered", "answer": map[string]any{"feedback": "Try a smaller step."}}, 0)
			require.Error(t, err)
			require.Empty(t, mutationsIn(eng))
		})
	}
}

func TestTypedPermissionGuidanceContinuesWithoutReplanningOrGranting(t *testing.T) {
	i, eng, approval, _ := guidanceFixture(t, workstate.ApprovalKindScopeElevation)
	hash, err := permission.Hash("workerHost", "exec", map[string]any{"cmd": "echo test", "cwd": "/fixture"})
	require.NoError(t, err)
	scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Test machine", Tool: "workerHost", Action: "exec", Command: "echo test", CWD: "/fixture", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
	subject := rowMap(approval, "subject")
	subject["computerPermission"] = scope.Map()
	approval["artifactHash"] = workstate.ArtifactHash(subject)
	nodes, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": approval["id"], "decision": "answered", "answer": map[string]any{"feedback": "Correct the command and ask again; no approval."}}, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	update := argsOf(t, eng, "updateWorkRun")
	require.Equal(t, "running", update["status"])
	require.Empty(t, update["waitingOn"])
	require.NotContains(t, update, "rerun")
	require.NotContains(t, update, "staleSteps")
	require.NotContains(t, update, "spent")
	decision := argsOf(t, eng, "decideWorkApproval")
	require.Equal(t, "answered", decision["decision"])
	require.Equal(t, map[string]any{"feedback": "Correct the command and ask again; no approval."}, decision["answer"])
}
