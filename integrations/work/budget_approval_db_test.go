package work

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBudgetApprovalAcrossReplicasPreservesConcurrentGoalCeilings(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	goalID := newRowId(goalConcept)
	a.write(t, "createWorkGoal", map[string]any{"goalId": goalID, "statement": "Research a document", "origin": "user",
		"ceilings": map[string]any{"maxModelCalls": 16, "maxRetries": 3, "costCeiling": 0.5}})
	approvals := make([]string, 2)
	for index, ceiling := range []string{"modelCalls", "retries"} {
		runID := newRowId(runConcept)
		approvals[index] = newRowId(approvalConcept)
		a.write(t, "createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "research", "templateFingerprint": "test", "mode": modeLive, "status": runStatusWaiting,
			"waitingOn": map[string]any{"kind": "approval", "subject": approvals[index]}, "startedAt": rfc(time.Now())})
		a.write(t, "updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 16, "retries": 3}})
		subject := map[string]any{"ceiling": ceiling, "limit": "recorded limit", "actual": "recorded usage"}
		a.write(t, "createWorkApproval", map[string]any{"approvalId": approvals[index], "runId": runID, "kind": "budget", "subject": subject,
			"artifactHash": artifactHashOf(subject), "requestedAt": rfc(time.Now())})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for index, replica := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(index int, replica *Integration) {
			defer wg.Done()
			_, err := replica.handleDecideApproval(actorCtx(a.owner), map[string]any{"approvalId": approvals[index], "decision": "approved", "answer": map[string]any{"newLimit": []int{64, 8}[index]}}, 0)
			errs <- err
		}(index, replica)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	rows := b.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))
	require.Len(t, rows, 1)
	require.Equal(t, map[string]any{"maxModelCalls": float64(64), "maxRetries": float64(8), "costCeiling": 0.5}, rows[0]["ceilings"])
}
