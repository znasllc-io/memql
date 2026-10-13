package work

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

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
