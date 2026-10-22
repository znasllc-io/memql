package work

import (
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestAcceptGoalResultsRequiresExactHumanReviewedResults(t *testing.T) {
	for _, tc := range []struct {
		name, status                  string
		foreign, stale, active, model bool
	}{
		{name: "success", status: "succeeded"}, {name: "owner accepts useful partial result", status: "failed"},
		{name: "foreign", status: "succeeded", foreign: true}, {name: "stale", status: "succeeded", stale: true},
		{name: "still active", status: "succeeded", active: true}, {name: "self acceptance", status: "succeeded", model: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, e := newTestIntegration(t)
			ctx := callerContext("u-alice")
			if tc.model {
				ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "r", GoalId: "g", OwnerUserId: "u-alice"})
			}
			owner := "u-alice"
			if tc.foreign {
				owner = "u-bob"
			}
			e.reply("workGoalForOwner", map[string]any{"id": "g", "ownerUserId": owner, "status": "open"})
			version := rfc(testNow)
			runs := []map[string]any{{"id": "r", "status": tc.status, "createdAt": version}}
			if tc.active {
				runs = append(runs, map[string]any{"id": "other", "status": "running", "createdAt": version})
			}
			e.reply("workRunsForGoal", runs...)
			if tc.stale {
				version = "old-version"
			}
			_, err := i.handleAcceptGoalResults(ctx, map[string]any{"goalId": "g", "runId": "r", "runVersion": version, "feedback": "This meets what I needed."}, 0)
			if tc.foreign || tc.stale || tc.active || tc.model {
				require.Error(t, err)
				require.Empty(t, e.callsTo("updateWorkGoal"))
				return
			}
			require.NoError(t, err)
			write := e.callTo(t, "updateWorkGoal").Args(t)
			require.Equal(t, "closed", write["status"])
			receipt := rowMap(write, "acceptance")
			require.Equal(t, "u-alice", receipt["acceptedBy"])
			require.Equal(t, "This meets what I needed.", receipt["feedback"])
			require.Equal(t, version, receipt["runVersion"])
		})
	}
}
func TestGoalAcceptanceRetryDoesNotRewriteFeedback(t *testing.T) {
	i, e := newTestIntegration(t)
	args := map[string]any{"goalId": "g", "runId": "r", "runVersion": rfc(testNow), "feedback": "Accepted"}
	e.reply("workGoalForOwner", map[string]any{"id": "g", "ownerUserId": "u-alice", "status": "closed", "acceptance": map[string]any{"runId": "r", "runVersion": rfc(testNow), "acceptedBy": "u-alice", "feedback": "Accepted"}})
	_, err := i.handleAcceptGoalResults(callerContext("u-alice"), args, 0)
	require.NoError(t, err)
	require.Empty(t, e.callsTo("updateWorkGoal"))
	args["feedback"] = "Changed"
	_, err = i.handleAcceptGoalResults(callerContext("u-alice"), args, 0)
	require.Error(t, err)
	require.Empty(t, e.callsTo("updateWorkGoal"))
}
func TestAgentCannotCloseItsOwnGoal(t *testing.T) {
	i, e := newTestIntegration(t)
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r", GoalId: "g", OwnerUserId: "u-alice"})
	_, err := i.handleCancelGoal(ctx, map[string]any{"goalId": "g"}, 0)
	require.Error(t, err)
	require.Empty(t, e.callsTo("updateWorkGoal"))
	require.Empty(t, e.callsTo("updateWorkRun"))
}
