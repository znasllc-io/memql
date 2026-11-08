package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type actorTestValidator func(context.Context, string, string, Role) error

func (f actorTestValidator) ValidateRoleTest(ctx context.Context, session, subject string, role Role) error {
	return f(ctx, session, subject, role)
}

func TestRoleTestBorrowingAndDetachedWorkRetainAuthority(t *testing.T) {
	persona := "v1:identity:user:role-test-actor"
	revoked := false
	SetRoleTestValidator(actorTestValidator(func(_ context.Context, session, subject string, role Role) error {
		if revoked || session != "session" || subject != persona {
			return errors.New("test ended")
		}
		return nil
	}))
	t.Cleanup(func() { SetRoleTestValidator(nil) })
	for _, role := range []Role{RoleReader, RoleWriter, RoleDeveloper, RoleAdmin} {
		for _, forwarded := range []bool{false, true} {
			t.Run(string(role)+map[bool]string{false: "/direct", true: "/forwarded"}[forwarded], func(t *testing.T) {
				expires := time.Now().Add(time.Minute).Truncate(time.Second)
				claims := map[string]any{"sub": persona, "role": string(role), "class": RoleTestClass, "role_test_session": "session", "exp": expires.Unix()}
				ac := &AccessContext{UserId: persona, Role: role, RoleTestSessionID: "session"}
				ctx := ContextWithAccess(ContextWithToken(ContextWithClaims(context.Background(), claims), BuildTokenInfo(claims)), ac)
				if forwarded {
					a, _ := ForwardedAuthorityForUser(ac, RoleTestClass, "", expires, time.Now())
					ctx = BindForwardedContext(context.Background(), a.Principal().Claims, ac, a)
				}
				for _, owner := range []string{persona, strings.TrimPrefix(persona, "v1:identity:user:")} {
					borrowed := ContextWithUserActor(ctx, owner)
					if borrowed != ctx {
						t.Fatal("same-person work replaced bounded authority")
					}
					if err := ValidateRoleTestContext(borrowed); err != nil {
						t.Fatal(err)
					}
				}
				wrong := ContextWithUserActor(ctx, "v1:identity:user:operator")
				if ValidateRoleTestContext(wrong) == nil || ValidateRoleTestContext(ContextWithUserActor(wrong, persona)) == nil {
					t.Fatal("different-owner borrow shed its refusal")
				}
				request, cancelRequest := context.WithCancel(ctx)
				detached, cancel, err := DetachUserWork(request, persona)
				if err != nil {
					t.Fatal(err)
				}
				defer cancel()
				cancelRequest()
				if detached.Err() != nil {
					t.Fatal("HTTP cancellation killed accepted analysis")
				}
				if deadline, ok := detached.Deadline(); !ok || !deadline.Equal(expires) {
					t.Fatal("analysis lost session expiry")
				}
				if got, _ := AccessFromContext(detached); got != ac {
					t.Fatal("analysis changed effective role")
				}
				revoked = true
				if ValidateRoleTestContext(detached) == nil {
					t.Fatal("ended analysis retained authority")
				}
				if _, _, err := DetachUserWork(ctx, persona); err == nil {
					t.Fatal("ended test started work")
				}
				revoked = false
			})
		}
	}
	missing := ContextWithAccess(context.Background(), &AccessContext{UserId: persona, Role: RoleReader})
	if _, _, err := DetachUserWork(missing, persona); err == nil {
		t.Fatal("unbounded persona started work")
	}
	if ValidateRoleTestContext(ContextWithUserActor(missing, "ordinary-user")) == nil {
		t.Fatal("unbounded persona escaped reserved identity")
	}
}

// testingCatalog is the seeds' ladder in miniature plus the custom roles the
// role-testing cases name. It resolves aliases the way the engine's catalog
// does, CanonicalSlug included, because a custom role is tested under its own
// slug and never under an alias.
type testingCatalog struct {
	*fakeCatalog
	canonical map[string]string
}

func (c testingCatalog) CanonicalSlug(slug string) string { return c.canonical[normalizeSlug(slug)] }

func installTestingCatalog(t *testing.T) {
	t.Helper()
	ranks := map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "viewer": 50,
		"release-captain": 350, "peer-lead": 300, "support-lead": 150, "retired-helper": 30, "site-moderator": 20}
	canonical := map[string]string{}
	for slug := range ranks {
		canonical[slug] = slug
	}
	for alias, slug := range map[string]string{"writer": "user", "reader": "viewer", "support": "support-lead"} {
		ranks[alias] = ranks[slug]
		canonical[alias] = slug
	}
	installFake(t, testingCatalog{
		fakeCatalog: &fakeCatalog{
			ranks:  ranks,
			scopes: map[string]string{"site-moderator": "org-fixture"},
			off:    map[string]bool{"retired-helper": true},
		},
		canonical: canonical,
	})
}

// TestAnOperatorMayTestACustomRoleBelowThem: the two operators who test the
// predefined roles test a custom one too, when it is active and ranks strictly
// below them. This is the role-level half; the service then requires the
// operator to hold everything the persona would (roletest's mayTest).
func TestAnOperatorMayTestACustomRoleBelowThem(t *testing.T) {
	installTestingCatalog(t)
	for _, c := range []struct {
		operator, target Role
		want             bool
		why              string
	}{
		{RoleDeveloper, "support-lead", true, "a custom role below a developer"},
		{RoleOwner, "support-lead", true, "a custom role below an owner"},
		{RoleDeveloper, "site-moderator", true, "an organization role below a developer"},
		{RoleOwner, "release-captain", true, "a custom role between developer and owner, for an owner"},
		{RoleDeveloper, "release-captain", false, "a custom role above a developer"},
		{RoleDeveloper, "peer-lead", false, "a custom role at a developer's own rank is not below it"},
		{RoleOwner, "retired-helper", false, "a retired role"},
		{RoleOwner, "unknown-role", false, "a slug the catalog does not carry"},
		{RoleOwner, "support", false, "an alias: a custom role is tested under its own slug"},
		{RoleOwner, "Support-Lead", false, "a spelling the catalog did not store"},
		{RoleOwner, "user", false, "the member rung, tested as writer"},
		{RoleOwner, "viewer", false, "the viewer rung, tested as reader"},
		{RoleOwner, RoleOwner, false, "nobody tests an owner"},
		{RoleAdmin, "site-moderator", false, "an admin is not a role-test operator"},
		{RoleWriter, "site-moderator", false, "a member is not a role-test operator"},
		{"support-lead", "site-moderator", false, "a custom role is not a role-test operator"},
		{RoleDeveloper, RoleReader, true, "the predefined four keep their exact rules"},
		{RoleDeveloper, RoleAdmin, false, "admin stays an owner's test"},
		{RoleOwner, RoleAdmin, true, "an owner tests admin"},
	} {
		if got := MayTestRole(c.operator, c.target); got != c.want {
			t.Errorf("MayTestRole(%s, %s) = %t, want %t: %s", c.operator, c.target, got, c.want, c.why)
		}
	}
	SetCapabilityCatalog(nil)
	if MayTestRole(RoleOwner, "support-lead") {
		t.Error("with no catalog installed there is no custom role to test")
	}
	if !MayTestRole(RoleOwner, RoleReader) {
		t.Error("the predefined roles stay testable before the catalog is readable")
	}
}
