package automations

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// A scheduled maintenance attempt that fails closes without parking only when
// the run holds ITS OWN automation's maintenance principal: a run of another
// automation keeps its approval and retry lifecycle whatever listed principal
// it holds.
func TestAFailedScheduledRunClosesOnlyUnderItsOwnMaintenancePrincipal(t *testing.T) {
	holding := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithAccess(context.Background(), ac)
	}
	own := auth.MaintenanceActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")
	other := auth.MaintenanceActor(auth.AutomationOriginEngine, "workerInvocationRetentionSweep")
	for _, tc := range []struct {
		why  string
		ctx  context.Context
		exec *AutomationExecution
		want bool
	}{
		{"its own principal, on a schedule", holding(own), &AutomationExecution{AutomationName: "sweepWaitingWorkRuns", TriggeredBy: "schedule"}, true},
		{"its own principal, not on a schedule", holding(own), &AutomationExecution{AutomationName: "sweepWaitingWorkRuns", TriggeredBy: "event"}, false},
		{"another listed automation's principal", holding(other), &AutomationExecution{AutomationName: "sweepWaitingWorkRuns", TriggeredBy: "schedule"}, false},
		{"an unlisted automation holding a listed principal", holding(own), &AutomationExecution{AutomationName: "zzUnlistedSweep", TriggeredBy: "schedule"}, false},
		{"the ordinary actor", holding(auth.AutomationActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")), &AutomationExecution{AutomationName: "sweepWaitingWorkRuns", TriggeredBy: "schedule"}, false},
	} {
		if got := scheduledUnderItsOwnMaintenancePrincipal(tc.ctx, tc.exec); got != tc.want {
			t.Errorf("%s: closes=%v, want %v", tc.why, got, tc.want)
		}
	}
}
