package auth

import "testing"

// The maintenance list is answered for the engine's own automations only, and
// IsMaintenancePrincipal recognises the principal itself, not a name.
func TestTheMaintenancePrincipalIsTheEnginesOwn(t *testing.T) {
	names := MaintenanceAutomationNames()
	if len(names) == 0 {
		t.Fatal("the maintenance list is empty, so nothing below is checked")
	}
	for _, name := range names {
		if MaintenanceActor(AutomationOriginOther, name) != nil || IsMaintenanceAutomation(AutomationOriginOther, name) {
			t.Errorf("%s is granted to an origin that is not the engine's own tree", name)
		}
		if MaintenanceActor(AutomationOrigin(0), name) != nil {
			t.Errorf("%s is granted to the zero origin; an origin nobody recorded is not the engine's", name)
		}
		ac := MaintenanceActor(AutomationOriginEngine, name)
		if ac == nil || !IsMaintenancePrincipal(ac) {
			t.Errorf("the engine's %s is not its maintenance principal: %+v", name, ac)
		}
	}
	for label, ac := range map[string]*AccessContext{
		"nobody":                            nil,
		"the ordinary automation actor":     {UserId: "system:automation:" + names[0], Role: RoleReader, Synthetic: true, Unranked: true},
		"the principal's id as a reader":    {UserId: "system:maintenance:" + names[0], Role: RoleReader, Synthetic: true, Unranked: true},
		"the principal's id, not synthetic": {UserId: "system:maintenance:" + names[0], Role: RoleOwner},
		"an unlisted maintenance id":        {UserId: "system:maintenance:routes", Role: RoleOwner, Synthetic: true, Unranked: true},
		"a person who owns the cluster":     {UserId: "user-owner", Role: RoleOwner},
	} {
		if IsMaintenancePrincipal(ac) {
			t.Errorf("%s reads as the maintenance principal", label)
		}
	}
}
