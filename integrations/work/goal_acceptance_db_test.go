package work

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestGoalAcceptanceDBPreservesOwnerDecisionAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": "succeeded", "outcome": map[string]any{"answer": "Reviewed deliverable"}})
	run := b.runRow(t, runID)
	args := map[string]any{"goalId": goalID, "runId": runID, "runVersion": testRunVersion(t, run), "feedback": "The result meets my request."}
	// A version changing while the review is open cannot be accepted silently.
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "outcome": map[string]any{"answer": "Revised deliverable"}})
	_, err := b.i.handleAcceptGoalResults(actorCtx(a.owner), args, 0)
	require.ErrorContains(t, err, "changed")
	run = b.runRow(t, runID)
	args["runVersion"] = testRunVersion(t, run)
	_, err = a.i.handleAcceptGoalResults(actorCtx(a.owner), args, 0)
	require.NoError(t, err)
	// Retry through a different engine must preserve the original receipt.
	before := a.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0]
	_, err = b.i.handleAcceptGoalResults(actorCtx(a.owner), args, 0)
	require.NoError(t, err)
	after := b.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0]
	require.Equal(t, before, after)
	require.Equal(t, "closed", rowString(after, "status"))
	receipt := rowMap(after, "acceptance")
	require.Equal(t, memql.BareShortId(a.owner), memql.BareShortId(rowString(receipt, "acceptedBy")))
	require.Equal(t, "The result meets my request.", rowString(receipt, "feedback"))
	_, err = b.i.handleAcceptGoalResults(actorCtx("foreign-reviewer"), args, 0)
	require.Error(t, err)

	// A model cannot turn an accepted goal back into running work. A new
	// explicit owner continuation can, and invalidates the old acceptance.
	modelCtx := common.ContextWithRun(actorCtx(a.owner), common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner})
	_, err = b.i.handleReplayRun(modelCtx, map[string]any{"runId": runID}, 0)
	require.ErrorContains(t, err, "interactive decision")
	_, err = b.i.handleReplayRun(actorCtx(a.owner), map[string]any{"runId": runID}, 0)
	require.NoError(t, err)
	reopened := a.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0]
	require.Equal(t, "open", rowString(reopened, "status"))
	require.Empty(t, rowMap(reopened, "acceptance"))
	_, err = a.i.handleAcceptGoalResults(actorCtx(a.owner), args, 0)
	require.ErrorContains(t, err, "still active")
}

func testRunVersion(t *testing.T, run map[string]any) string {
	t.Helper()
	at, ok := rowTime(run, "createdAt")
	require.True(t, ok)
	return rfc(at)
}
