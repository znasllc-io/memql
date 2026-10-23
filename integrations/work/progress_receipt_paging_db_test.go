package work

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

type progressArrivalEngine struct {
	Engine
	afterRead func()
}

func (e *progressArrivalEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	result, err := e.Engine.Execute(ctx, query)
	if err == nil && strings.Contains(query, "workProgressReceiptsForOwnerRun") && e.afterRead != nil && (result.GetMeta() == nil || !result.GetMeta().HasMore) {
		callback := e.afterRead
		e.afterRead = nil
		callback()
	}
	return result, err
}

func testProgressDBReceiptPaging(t *testing.T, a, b actsDB) {
	runID := a.openRun(t, nil)
	rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "research", Mode: common.RunModeLive}
	a.write(t, "updateWorkGoal", map[string]any{"goalId": rc.GoalId, "ceilings": map[string]any{"maxModelCalls": 96}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	ctx := actorCtx(a.owner)
	record := func(tool, finding string) {
		require.NoError(t, a.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: tool, NativeResultEvidence: workstate.ArtifactHash(map[string]any{"finding": finding})}))
	}
	record("readSource", "substantive result before polling")
	for n := 0; n < 105; n++ {
		record("recallWorkHistory", "a successful poll is not new work")
	}
	// A peer commits another result after the last page's snapshot, before
	// the first peer persists its renewal. Both real receipts must survive.
	b.i.engine = &progressArrivalEngine{Engine: b.eng, afterRead: func() { record("readSource", "concurrent substantive result") }}
	ok, err := b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12}, 0)
	require.NoError(t, err)
	require.True(t, ok, "older source evidence survives more than one page of non-progress successes")
	ok, err = a.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.True(t, ok, "the concurrent arrival was not discarded by the window write")
	ok, err = b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 36}, 0)
	require.NoError(t, err)
	require.False(t, ok, "neither page nor concurrent arrival may buy a second credit")
	require.Empty(t, b.query(t, "different-owner", "query "+call("workProgressReceiptsForOwnerRun", map[string]any{"runId": runID})))
}
