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
		// Spelled exactly like the principal, by hand: the system actor of a
		// listed name, and every exported field of the real one.
		"a system actor with a listed name": SystemActor("maintenance:" + names[0]),
		"the principal's fields, by hand":   {UserId: "system:maintenance:" + names[0], Role: RoleOwner, Synthetic: true, Unranked: true},
	} {
		if IsMaintenancePrincipal(ac) {
			t.Errorf("%s reads as the maintenance principal", label)
		}
	}
}

// The ordinary actor of an automation carries the engine's mark only when the
// automation is the engine's own, and a hand-built actor with its id carries
// none.
func TestTheEngineAutomationActorIsTheEnginesOwn(t *testing.T) {
	if !IsEngineAutomationActor(AutomationActor(AutomationOriginEngine, "zzSweep"), "zzSweep") {
		t.Fatal("the engine's automation's own actor is not recognised")
	}
	for label, ac := range map[string]*AccessContext{
		"another source, same name": AutomationActor(AutomationOriginOther, "zzSweep"),
		"another engine automation": AutomationActor(AutomationOriginEngine, "zzOther"),
		"built with its id":         {UserId: "system:automation:zzSweep", Role: RoleReader, Synthetic: true, Unranked: true},
		"a system actor spelled so": SystemActor("automation:zzSweep"),
		"nobody":                    nil,
	} {
		if IsEngineAutomationActor(ac, "zzSweep") {
			t.Errorf("%s reads as the engine's automation", label)
		}
	}
}
