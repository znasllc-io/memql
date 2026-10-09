package work

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestRerunCancelledGoalAcrossReplicas(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "draft", 0, 1, nil, "preserved draft", nil)
	a.finishRun(t, runID, []string{"draft"}, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	_, err := a.i.handleCancelGoal(actorCtx(a.owner), map[string]any{"goalId": goalID, "reason": "stop this attempt"}, 0)
	require.NoError(t, err)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusWaiting, "cancelRequested": true, "spent": map[string]any{"modelCalls": 3}})
	// Prime the receiving replica with the closed version, then advance its
	// timestamp beyond this host's clock. Reopening must beat both hazards.
	old, err := b.i.store().goalForOwner(actorCtx(a.owner), goalID)
	require.NoError(t, err)
	require.Equal(t, "closed", old["status"])
	future := time.Now().Add(time.Minute)
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "versionTime": rfc(future), "ceilings": map[string]any{"maxModelCalls": 64}})
	_, err = b.i.handleRerunStep(actorCtx(a.owner), map[string]any{"runId": runID, "stepKey": "draft"}, 0)
	require.NoError(t, err)
	// The original replica can now execute a consumer that requires an open
	// goal; no originating process-local continuation state is involved.
	goal, err := a.i.store().goalForOwner(memql.ContextWithFreshRead(actorCtx(a.owner)), goalID)
	require.NoError(t, err)
	require.Equal(t, "open", goal["status"])
	require.Equal(t, "", goal["closedAt"])
	require.Equal(t, "", goal["closeReason"])
	require.Equal(t, float64(64), rowMap(goal, "ceilings")["maxModelCalls"])
	run, err := a.i.store().runForOwner(memql.ContextWithFreshRead(actorCtx(a.owner)), runID)
	require.NoError(t, err)
	require.Equal(t, runStatusRunning, run["status"])
	require.Equal(t, false, run["cancelRequested"])
	require.Equal(t, float64(3), rowMap(run, "spent")["modelCalls"])
}
