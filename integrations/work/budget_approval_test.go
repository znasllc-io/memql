package work

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func budgetApprovalFixture(t *testing.T) (*Integration, *recordingEngine, *[]string) {
	t.Helper()
	i, eng := newTestIntegration(t)
	keys := []string{}
	i.decisionGate = func(_ context.Context, key ...string) (func(), error) {
		keys = append(keys, key...)
		return func() {}, nil
	}
	i.computerDecisionGate = func(ctx context.Context, id string) (func(), func(context.Context) error, error) {
		release, err := i.decisionGate(ctx, id)
		return release, func(context.Context) error { return nil }, err
	}
	subject := map[string]any{"ceiling": "modelCalls", "limit": "16 calls", "actual": "16 made"}
	eng.reply("workApprovalsForOwner", map[string]any{
		"id": "a1", "runId": "r1", "ownerUserId": "u-alice", "kind": "budget",
		"subject": subject, "artifactHash": artifactHashOf(subject),
	})
	eng.reply("workRunForOwner", map[string]any{
		"id": "r1", "goalId": "g1", "ownerUserId": "u-alice", "status": runStatusWaiting,
		"waitingOn": map[string]any{"kind": "approval", "subject": "a1"},
		"spent":     map[string]any{"modelCalls": 16, "tokensLocal": 82000},
	})
	eng.reply("workGoalForOwner", map[string]any{
		"id": "g1", "ownerUserId": "u-alice", "createdAt": testNow.Add(time.Hour),
		"ceilings": map[string]any{"maxModelCalls": 16, "costCeiling": 0.5, "maxRetries": 3},
	})
	return i, eng, &keys
}

func TestRetiredBudgetRequestsCannotRaiseLimitsOrResume(t *testing.T) {
	for _, answer := range []map[string]any{nil, {"newLimit": 64}, {"feedback": "Keep working"}} {
		i, eng, _ := budgetApprovalFixture(t)
		_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{"approvalId": "a1", "decision": "approved", "answer": answer}, 0)
		require.Error(t, err)
		require.Empty(t, eng.callsTo("updateWorkGoal"))
		require.Empty(t, eng.callsTo("decideWorkApproval"))
		require.Empty(t, eng.callsTo("updateWorkRun"))
	}
}
func TestRetiredBudgetRequestCanBeClosedWithoutLosingSpend(t *testing.T) {
	i, eng, _ := budgetApprovalFixture(t)
	_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{"approvalId": "a1", "decision": "rejected"}, 0)
	require.NoError(t, err)
	require.Empty(t, eng.callsTo("updateWorkGoal"))
	update := eng.callTo(t, "updateWorkRun").Args(t)
	require.Equal(t, runStatusFailed, update["status"])
	require.NotContains(t, update, "spent")
}
