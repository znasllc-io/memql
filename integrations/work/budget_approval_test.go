package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func budgetApprovalFixture(t *testing.T) (*Integration, *recordingEngine, *[]string) {
	t.Helper()
	i, eng := newTestIntegration(t)
	keys := []string{}
	i.decisionGate = func(_ context.Context, key string) (func(), error) {
		keys = append(keys, key)
		return func() {}, nil
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

func TestBudgetApprovalRaisesOnlyItsNamedCeilingBeforeResuming(t *testing.T) {
	i, eng, keys := budgetApprovalFixture(t)
	nodes, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "a1", "decision": "approved", "answer": map[string]any{"newLimit": 64},
	}, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["runResumed"])
	require.Equal(t, []string{"a1", "goal-budget-g1"}, *keys)
	write := eng.callTo(t, "updateWorkGoal")
	require.Equal(t, auth.OriginInternal, write.Origin)
	require.Equal(t, "u-alice", write.Actor)
	args := write.Args(t)
	require.Equal(t, map[string]any{"maxModelCalls": float64(64), "costCeiling": 0.5, "maxRetries": float64(3)}, args["ceilings"])
	version, err := time.Parse(time.RFC3339Nano, args["versionTime"].(string))
	require.NoError(t, err)
	require.True(t, version.After(testNow.Add(time.Hour)))
	for _, update := range eng.callsTo("updateWorkRun") {
		_, reset := update.Args(t)["spent"]
		require.False(t, reset, "resumption must preserve accumulated spend")
	}
	answer := eng.callTo(t, "decideWorkApproval").Args(t)["answer"]
	require.Equal(t, map[string]any{"newLimit": float64(64)}, answer)
}

func TestBudgetApprovalRefusesInvalidLimitsBeforeConsumingDecision(t *testing.T) {
	for _, value := range []any{nil, 0, -1, 16, 16.5, "64", 1e100} {
		i, eng, _ := budgetApprovalFixture(t)
		_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
			"approvalId": "a1", "decision": "approved", "answer": map[string]any{"newLimit": value},
		}, 0)
		require.Error(t, err, "value: %v", value)
		require.Empty(t, eng.callsTo("updateWorkGoal"))
		require.Empty(t, eng.callsTo("decideWorkApproval"))
	}
}

func TestBudgetApprovalKeepsPendingDecisionWhenBudgetWriteFails(t *testing.T) {
	i, eng, _ := budgetApprovalFixture(t)
	eng.refuse("updateWorkGoal", errors.New("database unavailable"))
	_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "a1", "decision": "approved", "answer": map[string]any{"newLimit": 64},
	}, 0)
	require.ErrorContains(t, err, "database unavailable")
	require.Empty(t, eng.callsTo("decideWorkApproval"))
	require.Empty(t, eng.callsTo("updateWorkRun"))
}

func TestBudgetApprovalCannotChangeAnotherOwnersGoal(t *testing.T) {
	i, eng, _ := budgetApprovalFixture(t)
	eng.reply("workGoalForOwner", map[string]any{"id": "g1", "ownerUserId": "u-bob"})
	_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "a1", "decision": "approved", "answer": map[string]any{"newLimit": 64},
	}, 0)
	require.ErrorContains(t, err, "owned goal is unavailable")
	require.Empty(t, eng.callsTo("updateWorkGoal"))
	require.Empty(t, eng.callsTo("decideWorkApproval"))
}

func TestBudgetApprovalCannotLowerANewerCeiling(t *testing.T) {
	i, eng, _ := budgetApprovalFixture(t)
	eng.reply("workGoalForOwner", map[string]any{"id": "g1", "ownerUserId": "u-alice", "ceilings": map[string]any{"maxModelCalls": 128}})
	_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "a1", "decision": "approved", "answer": map[string]any{"newLimit": 64},
	}, 0)
	require.ErrorContains(t, err, "already been raised")
	require.Empty(t, eng.callsTo("updateWorkGoal"))
	require.Empty(t, eng.callsTo("decideWorkApproval"))
}
