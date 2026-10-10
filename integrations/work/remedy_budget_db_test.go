package work

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestReplanBudgetContinuationSurvivesAReplicaChange(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			runID := a.openRun(t, nil)
			goalID := rowString(a.runRow(t, runID), "goalId")
			a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 48, "maxRetries": 2}})
			a.writeVersion(t, runID, runID, "fetch", 0, 1, nil, "saved research", nil)
			a.writeVersion(t, runID, runID, "draft", 1, 1, nil, "", map[string]any{"status": "failed", "errorCode": "step_failed"})
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusWaiting, "stepOrder": []string{"fetch", "draft"},
				"spent":     map[string]any{"modelCalls": 26, "retries": 2},
				"waitingOn": map[string]any{"kind": waitKindReplan, "subject": "draft", "reason": "divide unfinished work", "since": rfc(testNow)},
			})
			require.NoError(t, a.i.PauseReplanForBudget(context.Background(), a.owner, runID, "draft", remedyCeiling(runID)))
			parked := b.runRow(t, runID)
			require.Equal(t, 48, rowInt(rowMap(parked, "spent"), "modelCalls"))
			approvalID := rowString(rowMap(parked, "waitingOn"), "subject")
			_, err := b.i.handleDecideApproval(actorCtx(a.owner), map[string]any{"approvalId": approvalID, "decision": decision, "answer": map[string]any{"newLimit": 96}}, 0)
			require.NoError(t, err)
			run, err := a.i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(context.Background()), a.owner), runID)
			require.NoError(t, err)
			require.Equal(t, 48, rowInt(rowMap(run, "spent"), "modelCalls"))
			require.Equal(t, 2, rowInt(rowMap(run, "spent"), "retries"))
			require.Empty(t, rowMap(run, "rerun"))
			if decision == "approved" {
				require.Equal(t, runStatusWaiting, rowString(run, "status"))
				require.Equal(t, waitKindReplan, rowString(rowMap(run, "waitingOn"), "kind"))
				require.Equal(t, "draft", rowString(rowMap(run, "waitingOn"), "subject"))
				require.Equal(t, "divide unfinished work", rowString(rowMap(run, "waitingOn"), "reason"))
			} else {
				require.Equal(t, runStatusFailed, rowString(run, "status"))
				require.Empty(t, rowMap(run, "waitingOn"))
			}
			for _, step := range a.query(t, a.owner, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runID})) {
				if rowString(step, "key") == "fetch" {
					require.Equal(t, "done", rowString(step, "status"))
					require.Equal(t, 1, rowInt(step, "version"))
				}
			}
		})
	}
}
