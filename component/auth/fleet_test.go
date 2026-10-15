package auth

import (
	"context"
	"testing"
)

func TestFleetCannotBeGrantedToOtherRoles(t *testing.T) {
	SetCapabilityCatalog(nil)
	t.Cleanup(func() { SetCapabilityCatalog(nil) })
	installGrantFake(t, &countingGrantSource{user: map[string][]Grant{"person": {allow(VerbRead, "app:fleet"), allow(VerbRead, "app:fleet/logs")}}})
	for _, role := range []Role{RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader, "user", "viewer", "custom", ""} {
		t.Run(string(role), func(t *testing.T) {
			want := role == RoleOwner || role == RoleDeveloper
			subject := Subject{Role: role, UserId: "person"}
			for _, resource := range []string{"app:fleet", "app:fleet/logs"} {
				if got := CapableFor(context.Background(), subject, VerbRead, resource); got != want {
					t.Fatalf("%s: got %t want %t", resource, got, want)
				}
			}
			for _, decision := range EffectiveCapabilities(context.Background(), subject) {
				if (decision.Resource == "app:fleet" || decision.Resource == "app:fleet/logs") && decision.Held != want {
					t.Fatalf("menu disagrees: %+v", decision)
				}
			}
		})
	}
}

func TestFleetRoleMustRemainActiveAndUnscoped(t *testing.T) {
	cat := &fakeCatalog{ranks: map[string]int{"owner": 400, "developer": 300, "custom": 400}, off: map[string]bool{}, scopes: map[string]string{}}
	installFake(t, cat)
	if !MayManageFleet(RoleDeveloper) {
		t.Fatal("active developer denied")
	}
	if MayManageFleet("custom") {
		t.Fatal("custom rank acquired Fleet authority")
	}
	cat.off["developer"] = true
	if MayManageFleet(RoleDeveloper) {
		t.Fatal("retired developer admitted")
	}
	cat.off["developer"] = false
	cat.scopes["developer"] = "account"
	if MayManageFleet(RoleDeveloper) {
		t.Fatal("account-scoped developer admitted")
	}
}
