package automations

import (
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/work"
	"testing"
)

func TestUnfinishedStepUsesNativeReplanWithoutClassifyingQuotedGap(t *testing.T) {
	for _, gap := range []string{"Retain the second output; reuse the verified first output.", "The saved diagnostic mentions context_length_exceeded and insufficient_quota; finish the authorized save.", "The report quotes composition_failed, self_timeout and loop_depth_exceeded; retain the report."} {
		t.Run(gap, func(t *testing.T) {
			c := &countingClassifier{symptom: work.SymptomHuman}
			exec := &budgetAnsweringExecutor{run: goalRun("v1:work:run:unfinished", 1), goal: map[string]any{"id": "v1:work:goal:g1", "ceilings": map[string]any{"maxRetries": 3}}}
			calls := closeWithBudget(t, c, failedRun("v1:work:run:unfinished", "function failed: "+work.UnfinishedStepError(gap).Error(), 1, 0), exec)
			_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
			require.Equal(t, "waiting", args["status"])
			wait := args["waitingOn"].(map[string]any)
			require.Equal(t, WaitKindReplan, wait["kind"])
			require.Equal(t, "plan.unfinishedStep", wait["ruleId"])
			require.Contains(t, wait["reason"], gap)
			require.Equal(t, float64(2), args["spent"].(map[string]any)["retries"])
			require.Equal(t, float64(120), args["spent"].(map[string]any)["tokens"])
			require.False(t, anyCallNamed(calls, "createWorkApproval"))
			require.Zero(t, c.calls)
		})
	}
}
func TestUnfinishedStepPreservesActualTerminalAndRetryLimits(t *testing.T) {
	c := &countingClassifier{}
	message := work.UnfinishedStepError("Save the output.").Error()
	calls := closeWith(t, c, failedRun("v1:work:run:terminal", "composition_failed: "+message, 1, 0))
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	require.Equal(t, "failed", args["status"])
	require.Equal(t, "composition_failed", args["errorCode"])
	exec := &budgetAnsweringExecutor{run: goalRun("v1:work:run:limit", 3), goal: map[string]any{"id": "v1:work:goal:g1", "ceilings": map[string]any{"maxRetries": 3}}}
	calls = closeWithBudget(t, c, failedRun("v1:work:run:limit", message, 1, 0), exec)
	_, args = argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	require.Equal(t, WaitKindApproval, args["waitingOn"].(map[string]any)["kind"])
	require.NotContains(t, args, "spent")
	require.Zero(t, c.calls)
}
