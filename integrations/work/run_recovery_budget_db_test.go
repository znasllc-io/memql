package work

import (
	"testing"

	"github.com/stretchr/testify/require"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func testModelCallBudgetDBSeesPeerRaisesAndReceiptsBeforeHardExhaustion(t *testing.T, a, b actsDB) {
	ctx := actorCtx(a.owner)
	runID := a.openRun(t, nil)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	run := a.runRow(t, runID)
	goalID := rowString(run, "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96}})
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, Mode: common.RunModeLive}
	ga, gb := NewRunCeilings(a.eng, testLogger()), NewRunCeilings(b.eng, testLogger())
	for _, g := range []*RunCeilings{ga, gb} {
		limit, used, err := g.ModelCallBudget(ctx, rc)
		require.NoError(t, err)
		require.Equal(t, 12, limit)
		require.Zero(t, used)
	}
	// A warm ordinary query on A must not hide B's later approved total or
	// receipt. Include an unjournaled local answer and cache hit on A too.
	_, err := a.i.store().goalForOwner(ctx, goalID)
	require.NoError(t, err)
	_, err = a.i.store().query(ctx, "query "+call("workModelCallsPageForOwnerRun", map[string]any{"runId": runID}))
	require.NoError(t, err)
	localID := memqlengine.BareShortId(runID) + "-local"
	ga.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: localID, Served: "local", InputTokens: 20})
	ga.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	journal := NewModelJournal(b.eng)
	require.NoError(t, journal.Record(ctx, a.owner, memqlengine.JournaledCall{ReceiptID: memqlengine.BareShortId(runID) + "-peer", RunId: runID, StepKey: "work", RequestHash: "peer", Served: "local", InputTokens: 30}))
	for _, total := range []int{13, 19} {
		b.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96, "workloadOverrides": map[string]any{"maxModelCalls": total}}})
		for _, g := range []*RunCeilings{ga, gb, NewRunCeilings(b.eng, testLogger())} {
			limit, used, err := g.ModelCallBudget(ctx, rc)
			require.NoError(t, err)
			require.Equal(t, total, limit)
			if g == ga {
				require.Equal(t, 3, used, "local-only charges survive a fresh read")
			} else {
				require.Equal(t, 1, used, "another replica reconstructs durable receipts without local state")
			}
		}
	}
	// Recovery of the previously unjournaled answer keeps A's usage stable.
	require.NoError(t, journal.Record(ctx, a.owner, memqlengine.JournaledCall{ReceiptID: localID, RunId: runID, StepKey: "work", RequestHash: "local", Served: "local", InputTokens: 20}))
	_, used, err := ga.ModelCallBudget(ctx, rc)
	require.NoError(t, err)
	require.Equal(t, 3, used)
	_, used, err = gb.ModelCallBudget(ctx, rc)
	require.NoError(t, err)
	require.Equal(t, 2, used)
	after, err := a.i.store().runForOwner(memqlengine.ContextWithFreshRead(ctx), runID)
	require.NoError(t, err)
	require.Equal(t, run, after, "observing a budget cannot reset or rewrite the run")
}
