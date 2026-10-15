package auth

import (
	"context"
	"testing"
)

func TestPersonalAppsRespectIdentityAndEffectiveAccess(t *testing.T) {
	installFake(t, nil)
	installGrantFake(t, nil)
	for _, role := range []Role{RoleReader, RoleWriter, RoleAdmin, RoleDeveloper, RoleOwner} {
		ctx := ContextWithAccess(context.Background(), &AccessContext{UserId: "person", Role: role})
		for _, app := range []string{"files", "ask"} {
			if !CanManageOwnAppData(ctx, app) {
				t.Fatalf("%s cannot use personal %s", role, app)
			}
		}
		if CanManageOwnAppData(ctx, "email") {
			t.Fatal("personal exception widened to business data")
		}
	}
	for _, access := range []*AccessContext{
		{}, {UserId: "person", Role: "unknown"}, {UserId: "person", Role: RoleReader, Synthetic: true},
		{UserId: "person", Role: RoleReader, Unranked: true}, {Role: RoleReader},
	} {
		if CanManageOwnAppData(ContextWithAccess(context.Background(), access), "files") {
			t.Fatalf("invalid personal actor admitted: %+v", access)
		}
	}
	ctx := ContextWithAccess(context.Background(), &AccessContext{UserId: "person", Role: RoleReader})
	for _, resource := range []string{ResourceData, "app:files", "app:ask"} {
		SetGrantSource(&countingGrantSource{user: map[string][]Grant{"person": {deny(VerbRead, resource)}}})
		app := "files"
		if resource == "app:ask" {
			app = "ask"
		}
		if CanManageOwnAppData(ctx, app) {
			t.Fatalf("explicit denial of %s was ignored", resource)
		}
	}
	// Live catalogs resolve both seeded names and aliases. A custom ranked
	// role can use these same doors; no role-name allowlist may overrule it.
	SetGrantSource(nil)
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"viewer": 50, "personal-reviewer": 60},
		grants: map[string]map[VerbResource]bool{
			"viewer":            {{Verb: VerbRead, Resource: ResourceData}: true, {Verb: VerbRead, Resource: "app:files"}: true},
			"personal-reviewer": {{Verb: VerbRead, Resource: ResourceData}: true, {Verb: VerbRead, Resource: "app:files"}: true},
		},
	})
	for _, role := range []Role{"viewer", "personal-reviewer"} {
		ctx := ContextWithAccess(context.Background(), &AccessContext{UserId: "person", Role: role})
		if !CanManageOwnAppData(ctx, "files") || CanManageOwnAppData(ctx, "ask") {
			t.Fatalf("personal app access did not follow catalog grants for %s", role)
		}
	}
}
