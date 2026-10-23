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
	require.Equal(t, 12, limit, "recovery observes the same current workload window as admission")
	require.Zero(t, used)
	c.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: "local", Served: "local", InputTokens: 20})
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	eng.reply("workModelCallsPageForOwnerRun", usageRow("peer", "subscription", 30))
	for _, total := range []int{13, 19} {
		eng.reply("workGoalForOwner", map[string]any{"id": ceilingGoalId, "ownerUserId": ceilingOwner, "ceilings": map[string]any{"maxModelCalls": 96, "workloadOverrides": map[string]any{"maxModelCalls": total}}})
		for n := 0; n < 2; n++ {
			limit, used, err = c.ModelCallBudget(ctx, rc)
			require.NoError(t, err)
			require.Equal(t, total, limit, "a recorded allowance change is visible to recovery on a warm replica")
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

func TestModelCallBudgetObservesRenewedWindowWithoutChangingUsage(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 96})
	run := map[string]any{"id": ceilingRunId, "ownerUserId": ceilingOwner, "goalId": ceilingGoalId, "classification": map[string]any{"workload": "project"}}
	eng.reply("workRunForOwner", run)
	ctx, rc := context.Background(), aGoalBackedRun()
	limit, _, err := c.ModelCallBudget(ctx, rc)
	require.NoError(t, err)
	require.Equal(t, 96, limit)
	for n := 0; n < 122; n++ {
		c.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	}
	// Completed evidence has already renewed this museum-cataloguing run on
	// another replica. Recovery must observe the same window as admission,
	// rather than permanently suppressing summaries at the original 96.
	for _, extensions := range []int{4, 5} {
		run["classification"] = map[string]any{"workload": "project", "progressContinuation": map[string]any{"extensions": extensions}}
		eng.reply("workRunForOwner", run)
		limit, used, err := c.ModelCallBudget(ctx, rc)
		require.NoError(t, err)
		require.Equal(t, 96*(extensions+1), limit)
		require.Equal(t, 122, used, "observing a renewal does not refund cumulative usage")
	}
	for _, call := range eng.calls {
		require.NotContains(t, call.Query, "mutation ", "recovery observation cannot renew a window itself")
	}
}
