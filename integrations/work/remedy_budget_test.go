package work

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

func remedyCeiling(runID string) *memql.RunCeilingError {
	return &memql.RunCeilingError{RunId: runID,
		Breach: memql.RunCeilingBreach{Ceiling: "modelCalls", Limit: "48 calls", Actual: "48 made", Reason: "the run reached its model-call cap"},
		Spent:  memql.RunSpend{ModelCalls: 48, TokensLocal: 82000, WallClockMs: 9000000},
	}
}

func TestReplanBudgetApprovalResumesTheRemedyAcrossReplicas(t *testing.T) {
	i, eng, _ := newActsIntegration(t)
	run := actRunRow(runStatusWaiting, "fetch", "draft", "publish")
	run["waitingOn"] = map[string]any{"kind": waitKindReplan, "subject": "draft", "reason": "divide unfinished work", "since": rfc(testNow)}
	run["spent"] = map[string]any{"retries": 2, "events": 7, "modelCalls": 26}
	eng.reply("workRunForOwner", run)
	require.NoError(t, i.PauseReplanForBudget(context.Background(), actOwner, actRunId, "draft", remedyCeiling(actRunId)))
	approval := eng.callTo(t, "createWorkApproval").Args(t)
	require.Equal(t, work.ApprovalKindBudget, approval["kind"])
	require.Equal(t, work.ArtifactHash(rowMap(approval, "subject")), approval["artifactHash"])
	require.Equal(t, "draft", approval["stepKey"])
	park := eng.callTo(t, "updateWorkRun").Args(t)
	require.Equal(t, float64(48), rowMap(park, "spent")["modelCalls"])
	require.Equal(t, float64(2), rowMap(park, "spent")["retries"])
	require.Equal(t, float64(7), rowMap(park, "spent")["events"])
	for key, value := range park {
		run[key] = value
	}
	// A new handler has only persisted approval and run state, no remedy scope.
	peer, peerEng, _ := newActsIntegration(t)
	// The budget path nests two different keyed gates; the generic recorder
	// uses one mutex for every key. The database test exercises the real locks.
	peer.decisionGate = func(context.Context, string) (func(), error) { return func() {}, nil }
	approval["id"] = approval["approvalId"]
	approval["ownerUserId"] = actOwner
	peerEng.reply("workApprovalsForOwner", approval)
	peerEng.reply("workRunForOwner", run)
	peerEng.reply("workGoalForOwner", map[string]any{"id": actGoalId, "ownerUserId": actOwner, "ceilings": map[string]any{"maxModelCalls": 48, "maxRetries": 2}})
	_, err := peer.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": approval["id"], "decision": "approved", "answer": map[string]any{"newLimit": 96}}, 0)
	require.NoError(t, err)
	resume := peerEng.callTo(t, "updateWorkRun").Args(t)
	require.Equal(t, runStatusWaiting, resume["status"])
	require.Equal(t, map[string]any{"kind": waitKindReplan, "subject": "draft", "reason": "divide unfinished work", "since": rfc(testNow)}, rowMap(resume, "waitingOn"))
	for _, key := range []string{"spent", "rerun", "staleSteps", "stepOrder", "head"} {
		require.NotContains(t, resume, key)
	}
	for key, value := range resume {
		run[key] = value
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
	require.Equal(t, "divide unfinished work", calls[0].reason)
	require.Equal(t, actGoalId, calls[0].run.GoalId)
}

func TestReplanBudgetRefusesStaleWaitsAndUnsavedApprovals(t *testing.T) {
	for _, scenario := range []string{"wrong run", "wrong step", "cancelled", "moved", "write failed"} {
		t.Run(scenario, func(t *testing.T) {
			i, eng, _ := newActsIntegration(t)
			run := actRunRow(runStatusWaiting, "fetch", "draft")
			run["waitingOn"] = map[string]any{"kind": waitKindReplan, "subject": "draft", "since": rfc(testNow)}
			ceiling := remedyCeiling(actRunId)
			switch scenario {
			case "wrong run":
				ceiling.RunId = "other-run"
			case "wrong step":
				rowMap(run, "waitingOn")["subject"] = "fetch"
			case "cancelled":
				run["cancelRequested"] = true
			case "moved":
				run["status"] = runStatusRunning
			case "write failed":
				eng.refuse("createWorkApproval", errors.New("storage unavailable"))
			}
			eng.reply("workRunForOwner", run)
			require.Error(t, i.PauseReplanForBudget(context.Background(), actOwner, actRunId, "draft", ceiling))
			require.Empty(t, eng.callsTo("updateWorkRun"))
			if scenario != "write failed" {
				require.Empty(t, eng.callsTo("createWorkApproval"))
			}
		})
	}
}
