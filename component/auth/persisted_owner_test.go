package auth

import (
	"context"
	"testing"
	"time"
)

func TestDurableOwnerResolvesCurrentRoleAndRetainsIntakeCeiling(t *testing.T) {
	owner := "v1:identity:user:alice"
	parent := ContextWithInternalOrigin(ContextWithAccess(context.Background(), &AccessContext{UserId: "system:maintenance", Role: RoleOwner, Synthetic: true}))
	for _, test := range []struct{ current, ceiling, want Role }{{RoleOwner, RoleOwner, RoleOwner}, {RoleOwner, RoleReader, RoleReader}, {RoleReader, RoleOwner, RoleReader}, {RoleWriter, RoleWriter, RoleWriter}} {
		resolver := NewIdentityResolver(QueryRunnerFunc(func(ctx context.Context, q string) (any, error) {
			if OriginFromContext(ctx) != OriginInternal {
				t.Fatal("identity lookup lost internal origin")
			}
			return map[string]any{"role": string(test.current)}, nil
		}), nil)
		grant := map[string]any{"roleCeiling": string(test.ceiling), "credentialClass": ForwardedClassUser}
		ctx, err := ContextWithPersistedOwner(parent, owner, grant, resolver)
		if err != nil {
			t.Fatal(err)
		}
		assertion, ok := ForwardedAuthorityFromContext(ctx)
		if !ok {
			t.Fatal("no assertion")
		}
		received, err := VerifyForwardedAuthority(assertion, time.Now())
		if err != nil || received.Role != test.want || received.UserId != owner || received.Synthetic || received.Unranked || OriginFromContext(ctx) != OriginClient {
			t.Fatalf("bad receive: %+v %v", received, err)
		}
	}
}
func TestDurableOwnerRefusesExpiredBadgeAndForgedCeiling(t *testing.T) {
	for _, grant := range []map[string]any{
		{"roleCeiling": "owner", "credentialClass": "invented"},
		{"roleCeiling": "root", "credentialClass": ForwardedClassUser},
		{"roleCeiling": "reader", "credentialClass": ForwardedClassBadge, "expiresAt": time.Now().Add(-time.Second).Format(time.RFC3339Nano)},
	} {
		if _, err := ContextWithPersistedOwner(context.Background(), "v1:identity:user:alice", grant, nil); err == nil {
			t.Fatalf("accepted %+v", grant)
		}
	}
}
func TestCaptureDurableAuthorityKeepsBadgeCeilingAndExpiry(t *testing.T) {
	access := &AccessContext{UserId: "v1:identity:user:alice", Role: RoleReader}
	expires := time.Now().Add(time.Minute)
	assertion, _ := ForwardedAuthorityForUser(access, ForwardedClassBadge, RoleReader, expires, time.Now())
	verified, _ := VerifyForwardedAuthority(assertion, time.Now())
	ctx := BindForwardedContext(context.Background(), assertion.Principal().Claims, verified, assertion)
	grant, err := CaptureExecutionAuthority(ctx)
	if err != nil || grant["roleCeiling"] != "reader" || grant["credentialClass"] != ForwardedClassBadge || grant["expiresAt"] != expires.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
}

// TestAStandInRoleSaysSo: the two actors whose Role is a stand-in for the
// person -- borrowed authority, and work restored without a captured grant --
// mark it, and a grant-backed restoration does not (memql#5438). A tool's
// @requiresRank reads the mark to judge the person rather than "writer".
func TestAStandInRoleSaysSo(t *testing.T) {
	owner := "v1:identity:user:alice"
	borrowed, _ := AccessFromContext(ContextWithUserActor(context.Background(), owner))
	if borrowed == nil || !borrowed.RoleStandIn || borrowed.Role != RoleWriter {
		t.Fatalf("borrowed authority = %+v; want a writer stand-in", borrowed)
	}

	ungranted, err := ContextWithPersistedOwner(context.Background(), owner, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ac, _ := AccessFromContext(ungranted); ac == nil || !ac.RoleStandIn || ac.Role != RoleWriter {
		t.Fatalf("work with no captured grant = %+v; want a writer stand-in", ac)
	}

	resolver := NewIdentityResolver(QueryRunnerFunc(func(context.Context, string) (any, error) {
		return map[string]any{"role": string(RoleAdmin)}, nil
	}), nil)
	granted, err := ContextWithPersistedOwner(context.Background(), owner,
		map[string]any{"roleCeiling": string(RoleAdmin), "credentialClass": ForwardedClassUser}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if ac, _ := AccessFromContext(granted); ac == nil || ac.RoleStandIn || ac.Role != RoleAdmin {
		t.Fatalf("work with a captured grant = %+v; want the resolved admin role, not a stand-in", ac)
	}
}
