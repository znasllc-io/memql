package sitehealth

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The sweep admits the maintenance principal of checkDeployableHealth, as its
// constructor minted it, and nobody else: not the automation's ordinary actor,
// and not an actor spelled with the principal's id.
func TestTheSweepAdmitsOnlyTheMintedMaintenancePrincipal(t *testing.T) {
	const refusal = "require the engine maintenance principal"
	real := auth.MaintenanceActor(auth.AutomationOriginEngine, "checkDeployableHealth")
	// Every exported field the real one has, set by hand.
	forged := &auth.AccessContext{UserId: real.UserId, Role: real.Role, Unranked: real.Unranked, Synthetic: real.Synthetic}
	i := &Integration{}
	for name, ac := range map[string]*auth.AccessContext{
		"a system actor with the listed name":    auth.SystemActor("maintenance:checkDeployableHealth"),
		"an actor built with the principal's id": forged,
		"the automation's ordinary reader actor": auth.AutomationActor(auth.AutomationOriginEngine, "checkDeployableHealth"),
	} {
		if _, err := i.sweep(auth.ContextWithAccess(context.Background(), ac), nil, 0); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: want the principal refusal, got %v", name, err)
		}
	}
	// The real principal passes the check; the storage that is not wired here
	// is what refuses next.
	if _, err := i.sweep(auth.ContextWithAccess(context.Background(), real), nil, 0); err == nil || strings.Contains(err.Error(), refusal) {
		t.Errorf("the real maintenance principal was refused: %v", err)
	}
}
