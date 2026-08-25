package auth

import (
	"context"
	"testing"
)

func TestAnonymousActorCarriesTheAnonymousRoleAndFlag(t *testing.T) {
	ac := AnonymousActor()
	if ac == nil {
		t.Fatal("AnonymousActor() returned nil")
	}
	if ac.Role != RoleAnonymous {
		t.Errorf("Role = %q, want %q", ac.Role, RoleAnonymous)
	}
	if !ac.IsAnonymous {
		t.Error("IsAnonymous is false")
	}
	if ac.UserId != AnonymousUserId {
		t.Errorf("UserId = %q, want %q", ac.UserId, AnonymousUserId)
	}
	if ac.UserId == "" {
		t.Error("UserId is blank -- several engine surfaces read a blank UserId as 'no identity' and refuse, which is the wrong answer for a caller authorized by a different rule")
	}
	if ac.IsClusterOwner() {
		t.Error("the anonymous actor reads as a cluster owner")
	}
	if !ac.IsAnonymousActor() {
		t.Error("AnonymousActor() does not satisfy IsAnonymousActor()")
	}
}

// TestEveryAnonymousActorIsTheSameActor is the cache-key property at its
// source. actorCacheKeyComponent keys on UserId+Role+IdentityId, so a
// per-visitor field here would give every visitor their own cache entry --
// a read that stays CORRECT and silently stops being cached, which is the
// kind of regression nothing notices.
func TestEveryAnonymousActorIsTheSameActor(t *testing.T) {
	a, b := AnonymousActor(), AnonymousActor()
	if a.UserId != b.UserId || a.Role != b.Role || a.IdentityId != b.IdentityId {
		t.Fatalf("two anonymous actors differ: %+v vs %+v -- public reads would no longer share one cache key", a, b)
	}
	if a.IdentityId != "" {
		t.Errorf("IdentityId = %q; an anonymous caller authenticated with no credential, so it must stay empty", a.IdentityId)
	}
}

// TestRoleAnonymousIsNotAUserRoleAndIsNotPrivileged mirrors the connector
// actor's equivalent gate. A role a user can be assigned is a role an
// identity can be issued with.
func TestRoleAnonymousIsNotAUserRoleAndIsNotPrivileged(t *testing.T) {
	for _, r := range ValidRoles() {
		if r == RoleAnonymous {
			t.Fatal("RoleAnonymous appears in ValidRoles() -- a user could then be assigned it, and an identity issued with it")
		}
	}
	if got, want := RoleLevel(RoleAnonymous), RoleLevel(RoleReader); got != want {
		t.Errorf("RoleLevel(RoleAnonymous) = %d, want %d (the least privileged tier)", got, want)
	}
}

func TestAMalformedAnonymousActorIsNotAnonymous(t *testing.T) {
	for name, ac := range map[string]*AccessContext{
		"nil":                       nil,
		"the flag without the role": {UserId: AnonymousUserId, Role: RoleReader, IsAnonymous: true},
		"the role without the flag": {UserId: AnonymousUserId, Role: RoleAnonymous},
		"an ordinary user":          {UserId: "u1", Role: RoleWriter},
	} {
		if ac.IsAnonymousActor() {
			t.Errorf("%s reads as the anonymous actor -- a half-built actor must DENY, since the anonymous rule is the only thing that would have granted it anything", name)
		}
	}
	if IsAnonymousActor(context.Background()) {
		t.Error("a context with no actor reads as anonymous -- absence must stay absence, or every unauthenticated internal call becomes a public reader")
	}
}

// TestPublicReadsDefaultsOffAndRefusesATypo. The default is the whole
// safety story on every cluster that exists today, and a typo must never
// open an anonymous surface.
func TestPublicReadsDefaultsOffAndRefusesATypo(t *testing.T) {
	t.Setenv(PublicReadsEnabledEnv, "")
	if PublicReadsEnabled() {
		t.Fatal("public reads are enabled with the variable unset -- that is a behaviour change on every existing cluster")
	}

	for _, v := range []string{"false", "0", "no", "off", "yes please", "TRUEish", "  "} {
		t.Setenv(PublicReadsEnabledEnv, v)
		if PublicReadsEnabled() {
			t.Errorf("PublicReadsEnabled() is true for %q -- an anonymous surface must be something an operator turned on deliberately", v)
		}
	}
	for _, v := range []string{"true", "1", "TRUE", " true "} {
		t.Setenv(PublicReadsEnabledEnv, v)
		if !PublicReadsEnabled() {
			t.Errorf("PublicReadsEnabled() is false for %q -- an operator who opted in was ignored", v)
		}
	}
}

func TestContextWithAnonymousActorRoundTrips(t *testing.T) {
	ctx := ContextWithAnonymousActor(context.Background())
	if !IsAnonymousActor(ctx) {
		t.Fatal("ContextWithAnonymousActor did not produce an anonymous context")
	}
	ac, ok := AccessFromContext(ctx)
	if !ok || ac.UserId != AnonymousUserId {
		t.Fatalf("AccessFromContext = %+v, ok=%v", ac, ok)
	}
	// It is not a connector, and a connector is not anonymous. The two
	// named non-request actors must stay distinguishable: each has its own
	// admission rule, and either one answering for the other is a bypass.
	if ac.IsConnector() {
		t.Error("the anonymous actor reads as a connector")
	}
	if IsAnonymousActor(ContextWithConnectorActor(context.Background(), "shopify")) {
		t.Error("a connector actor reads as anonymous")
	}
}
