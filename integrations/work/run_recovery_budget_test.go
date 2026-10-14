package work

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestModelCallBudgetRefreshesApprovedTotalsWithoutRefundingUsage(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 96})
	eng.reply("workRunForOwner", map[string]any{"id": ceilingRunId, "ownerUserId": ceilingOwner, "goalId": ceilingGoalId, "classification": map[string]any{"workload": "lookup"}})
	ctx, rc := context.Background(), aGoalBackedRun()
	limit, used, err := c.ModelCallBudget(ctx, rc)
	require.NoError(t, err)
	require.Equal(t, 12, limit, "the workload estimate, not the larger declared cap, governs")
	require.Zero(t, used)
	c.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: "local", Served: "local", InputTokens: 20})
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	eng.reply("workModelCallsPageForOwnerRun", usageRow("peer", "subscription", 30))
	for _, total := range []int{13, 19} {
		eng.reply("workGoalForOwner", map[string]any{"id": ceilingGoalId, "ownerUserId": ceilingOwner, "ceilings": map[string]any{"maxModelCalls": 96, "workloadOverrides": map[string]any{"maxModelCalls": total}}})
		for n := 0; n < 2; n++ {
			limit, used, err = c.ModelCallBudget(ctx, rc)
			require.NoError(t, err)
			require.Equal(t, total, limit, "a raise is visible before this replica reaches its old hard cap")
			require.Equal(t, 3, used, "keep peer, unjournaled local, and cache charges exactly once")
		}
	}
	eng.reply("workModelCallsPageForOwnerRun", usageRow("peer", "subscription", 30), usageRow("local", "local", 20))
	_, used, err = c.ModelCallBudget(ctx, rc)
	require.NoError(t, err)
	require.Equal(t, 3, used, "a recovered local receipt does not charge twice")
	spent := c.Spent(ctx, rc)
	require.Equal(t, 20, spent.TokensLocal)
	require.Equal(t, 30, spent.TokensSubscription)
	for _, call := range eng.calls {
		require.NotContains(t, call.Query, "mutation ", "observation cannot alter durable state")
	}
}

func TestModelCallBudgetNeverTreatsUnreadableUsageAsUnlimited(t *testing.T) {
	for _, target := range []string{"workRunForOwner", "workGoalForOwner", "workModelCallsPageForOwnerRun"} {
		t.Run(target, func(t *testing.T) {
			c, eng := newTestCeilings(t)
			answerRows(eng, map[string]any{"maxModelCalls": 12})
			ctx, rc := context.Background(), aGoalBackedRun()
			_, _, err := c.ModelCallBudget(ctx, rc)
			require.NoError(t, err)
			c.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
			eng.refuse(target, errors.New("reader unavailable"))
			_, _, err = c.ModelCallBudget(ctx, rc)
			require.Error(t, err)
			require.Equal(t, 1, c.Spent(ctx, rc).ModelCalls)
		})
	}
	var absent *RunCeilings
	limit, used, err := absent.ModelCallBudget(context.Background(), common.RunContext{})
	require.NoError(t, err)
	require.Zero(t, limit)
	require.Zero(t, used)
}
