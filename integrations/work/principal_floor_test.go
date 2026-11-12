package work

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The sweep floor admits the maintenance principal as its constructor minted
// it, and a person who owns the cluster; a synthetic owner spelled like the
// principal, and an automation's ordinary reader actor, are refused.
func TestTheSweepFloorDecidesOnTheMintedPrincipal(t *testing.T) {
	real := auth.MaintenanceActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")
	for name, ac := range map[string]*auth.AccessContext{
		"a system actor with a listed name":      auth.SystemActor("maintenance:sweepWaitingWorkRuns"),
		"an actor built with the principal's id": {UserId: real.UserId, Role: real.Role, Unranked: true, Synthetic: true},
		"the automation's ordinary reader actor": auth.AutomationActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns"),
	} {
		if err := requireClusterOwner(auth.ContextWithAccess(context.Background(), ac)); err == nil {
			t.Errorf("%s cleared the sweep floor", name)
		}
	}
	for name, ac := range map[string]*auth.AccessContext{
		"the maintenance principal":     real,
		"a person who owns the cluster": {UserId: "v1:identity:user:owner", Role: auth.RoleOwner},
	} {
		if err := requireClusterOwner(auth.ContextWithAccess(context.Background(), ac)); err != nil {
			t.Errorf("%s was refused the sweep floor: %v", name, err)
		}
	}
}
