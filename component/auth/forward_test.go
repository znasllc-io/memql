package auth

import (
	"context"
	"testing"
	"time"
)

// Tests for the mesh forwarded-auth contract (memql#3205).
//
// The contract's whole claim is that a receiver refuses when it cannot PROVE
// the sender's ceiling was applied. Most of what follows is therefore a
// refusal matrix: each case is a way the assertion could be unprovable, and
// each must be an error rather than a degraded accept.

func TestValidateRefusesAnAbsentAuthority(t *testing.T) {
	var missing *ForwardedAuthority
	if err := missing.Validate(time.Now()); err == nil {
		t.Fatal("a nil authority must be refused; inferring safety from absence is the defect this contract replaces")
	}
	// Callers distinguish "the producer forgot" from "the assertion is bad".
	if err := missing.Validate(time.Now()); err != ErrAuthorityMissing {
		t.Errorf("nil authority error = %v, want ErrAuthorityMissing", err)
	}
}

func TestValidateRefusalMatrix(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	cases := []struct {
		name      string
		authority *ForwardedAuthority
		wantErr   bool
		why       string
	}{
		{
			name:      "unknown kind",
			authority: &ForwardedAuthority{Kind: "administrator"},
			wantErr:   true,
			why:       "an unrecognised kind must not fall through to some default binding",
		},
		{
			name:      "empty kind",
			authority: &ForwardedAuthority{},
			wantErr:   true,
			why:       "the zero value must never be an accepted assertion -- that is what makes the field effectively mandatory",
		},
		{
			name:      "user without a subject",
			authority: &ForwardedAuthority{Kind: AuthorityKindUser, Role: RoleWriter},
			wantErr:   true,
			why:       "a principal-bearing kind with no principal is the silent zero-rows failure",
		},
		{
			name:      "user with an unrecognised role",
			authority: &ForwardedAuthority{Kind: AuthorityKindUser, UserId: "v1:identity:user:a", Role: "superuser"},
			wantErr:   true,
			why:       "an unknown role means the two sides disagree about the role set; silently clamping would hide that",
		},
		{
			name:      "valid user",
			authority: &ForwardedAuthority{Kind: AuthorityKindUser, UserId: "v1:identity:user:a", Role: RoleWriter},
			wantErr:   false,
		},
		{
			name: "badge without an expiry",
			authority: &ForwardedAuthority{
				Kind: AuthorityKindBadge, UserId: "v1:identity:user:a", Role: RoleReader,
			},
			wantErr: true,
			why:     "a badge whose expiry did not cross cannot be gated on the worker at all",
		},
		{
			name: "expired badge",
			authority: &ForwardedAuthority{
				Kind: AuthorityKindBadge, UserId: "v1:identity:user:a", Role: RoleReader,
				BadgeExpires: now.Add(-time.Second),
			},
			wantErr: true,
			why:     "a walked-away kiosk's grant is rejected on the direct stream; it must be rejected on a forward too",
		},
		{
			name: "live badge",
			authority: &ForwardedAuthority{
				Kind: AuthorityKindBadge, UserId: "v1:identity:user:a", Role: RoleReader,
				BadgeExpires: now.Add(time.Minute),
			},
			wantErr: false,
		},
		{
			name:      "system naming an unknown actor",
			authority: &ForwardedAuthority{Kind: AuthorityKindSystem, UserId: "system:attacker"},
			wantErr:   true,
			why:       "the sender picks among known service principals; it must not be able to invent one",
		},
		{
			name:      "system naming an allowlisted actor",
			authority: &ForwardedAuthority{Kind: AuthorityKindSystem, UserId: SystemActorPlanner},
			wantErr:   false,
		},
		{
			name:      "internal",
			authority: &ForwardedAuthority{Kind: AuthorityKindInternal},
			wantErr:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.authority.Validate(now)
			if tc.wantErr && err == nil {
				t.Fatalf("expected refusal: %s", tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
		})
	}
}

// TestBadgeExpiryIsEnforcedAtTheInstant pins the boundary rather than a fuzzy
// "expired eventually": the gate is `now < exp`, matching badgeGate on the
// direct path, so a grant is dead the moment it expires and not a tick later.
func TestBadgeExpiryIsEnforcedAtTheInstant(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	a := &ForwardedAuthority{
		Kind: AuthorityKindBadge, UserId: "v1:identity:user:a", Role: RoleReader, BadgeExpires: exp,
	}
	if err := a.Validate(exp.Add(-time.Nanosecond)); err != nil {
		t.Errorf("a grant one nanosecond before exp must be live, got %v", err)
	}
	if err := a.Validate(exp); err == nil {
		t.Error("a grant AT exp must be refused; the direct path's badgeGate uses the same strict boundary")
	}
}

// TestSystemAuthorityPinsTheRoleReceiverSide is the property that makes the
// system kind safe: the wire names WHICH service principal, never what it may
// do. The predecessor put role:"system" on the wire and the receiver trusted
// it through FallbackFromClaims.
func TestSystemAuthorityPinsTheRoleReceiverSide(t *testing.T) {
	hostile := &ForwardedAuthority{
		Kind:   AuthorityKindSystem,
		UserId: SystemActorCognition,
		Role:   RoleOwner, // the sender asks for owner
	}
	if err := hostile.Validate(time.Now()); err != nil {
		t.Fatalf("an allowlisted system actor must be accepted: %v", err)
	}
	ac := hostile.AccessContext()
	if ac == nil {
		t.Fatal("system authority resolved no AccessContext")
	}
	if ac.Role != SystemActorRole {
		t.Errorf("system role = %q, want the receiver-pinned %q -- the sender must not be able to choose", ac.Role, SystemActorRole)
	}
	if ac.IsClusterOwner() {
		t.Error("a system forward asking for owner resolved as cluster owner; the role must be pinned, not carried")
	}
	if ac.UserId != SystemActorCognition {
		t.Errorf("system subject = %q, want %q -- the audit identity must survive so a stamped row is attributable", ac.UserId, SystemActorCognition)
	}
}

// TestSystemActorsStayDistinct guards the reason the subject is constrained
// rather than pinned: the planner and cognition deliberately carry different
// ids so a stamped row can be attributed to the integration that produced it.
func TestSystemActorsStayDistinct(t *testing.T) {
	if SystemActorPlanner == SystemActorCognition {
		t.Fatal("the two system actors collapsed into one; audit can no longer tell planner-stamped rows from cognition-stamped ones")
	}
	for _, id := range []string{SystemActorPlanner, SystemActorCognition} {
		if !IsKnownSystemActor(id) {
			t.Errorf("%q is not in the allowlist, so its own forwards would be refused", id)
		}
	}
	if IsKnownSystemActor("system:anything-else") {
		t.Error("the allowlist admitted an unknown actor")
	}
}

// TestPrincipalAuthoritySelectsBadgeOnlyWithAnExpiry pins the producer-side
// rule that a badge is exactly "a user plus an enforceable deadline".
func TestPrincipalAuthoritySelectsBadgeOnlyWithAnExpiry(t *testing.T) {
	ac := &AccessContext{UserId: "v1:identity:user:a", PrimaryEmail: "a@example.com", Role: RoleReader}

	plain, err := PrincipalAuthority(ac, time.Time{}, false)
	if err != nil {
		t.Fatalf("PrincipalAuthority: %v", err)
	}
	if plain.Kind != AuthorityKindUser {
		t.Errorf("kind = %q, want user for a non-badge session", plain.Kind)
	}

	exp := time.Now().Add(time.Minute)
	badge, err := PrincipalAuthority(ac, exp, false)
	if err != nil {
		t.Fatalf("PrincipalAuthority: %v", err)
	}
	if badge.Kind != AuthorityKindBadge {
		t.Errorf("kind = %q, want badge when the session carries an expiry", badge.Kind)
	}
	if !badge.BadgeExpires.Equal(exp) {
		t.Errorf("badge expiry = %v, want %v", badge.BadgeExpires, exp)
	}

	if _, err := PrincipalAuthority(nil, time.Time{}, false); err == nil {
		t.Error("building a principal authority from a nil AccessContext must fail rather than emit an empty principal")
	}
	if _, err := PrincipalAuthority(&AccessContext{}, time.Time{}, false); err == nil {
		t.Error("building a principal authority from an empty AccessContext must fail")
	}
}

// TestContextWithForwardedAuthorityBindsTheActor is the fix for the original
// defect: the receiver must set an AccessContext, because that is what every
// engine actor surface reads (resolveActorPath, the spec evaluator, mutation
// templates, the automation actor envelope). The predecessor set only
// TokenInfo + claims, so actor.userId resolved to "".
func TestContextWithForwardedAuthorityBindsTheActor(t *testing.T) {
	authority := &ForwardedAuthority{
		Kind:         AuthorityKindUser,
		UserId:       "v1:identity:user:alice",
		PrimaryEmail: "alice@example.com",
		Role:         RoleWriter,
	}
	ctx := ContextWithForwardedAuthority(context.Background(), authority)

	ac, ok := AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext on the forwarded ctx; every actor-gated construct would return zero rows")
	}
	if ac.UserId != "v1:identity:user:alice" || ac.Role != RoleWriter {
		t.Errorf("bound actor = %+v, want alice/writer", ac)
	}
	if actor := ActorFromContext(ctx); actor == "" {
		t.Error("ActorFromContext is empty; mutations would fail with \"no actor found in context\"")
	}
	// UserIdentityFromContext resolves through TokenInfo, and the many
	// component/memql/*_validation.go actor validators reachable on a worker
	// read it, so it has to be populated too.
	if id, err := UserIdentityFromContext(ctx); err != nil || id.Subject != "v1:identity:user:alice" {
		t.Errorf("UserIdentityFromContext = %+v, %v; want alice", id, err)
	}
}

// TestSynthesizedClaimsCarryNoCeilingInputs guards against the receiver being
// handed the materials to re-clamp an already-final role. The claims the
// worker sees are DERIVED from the decision, so `class` and `role_ceiling`
// must not appear -- if they did, a downstream applyBadgeRoleCeiling could
// clamp a second time.
func TestSynthesizedClaimsCarryNoCeilingInputs(t *testing.T) {
	authority := &ForwardedAuthority{
		Kind:         AuthorityKindBadge,
		UserId:       "v1:identity:user:operator",
		PrimaryEmail: "operator@example.com",
		Role:         RoleReader,
		BadgeExpires: time.Now().Add(time.Minute),
	}
	claims := authority.synthesizedClaims()
	for _, forbidden := range []string{"class", "role_ceiling", "exp"} {
		if _, present := claims[forbidden]; present {
			t.Errorf("synthesized claims carry %q; the role is already final and must never be re-derived", forbidden)
		}
	}
	if claims["role"] != string(RoleReader) {
		t.Errorf("synthesized role = %v, want the already-clamped reader", claims["role"])
	}

	// And the round trip must not re-clamp: feeding these claims back through
	// the resolver's fallback yields the same role rather than a second clamp.
	if got := FallbackFromClaims(claims); got.Role != RoleReader {
		t.Errorf("re-resolving the synthesized claims changed the role to %q", got.Role)
	}
}

// TestForwardedAuthoritySurvivesForTheNextHop is the multi-hop property.
//
// BFF -> cognition -> agent is two hops. If hop two rebuilt the authority from
// the AccessContext it would keep the clamped role but silently drop
// BadgeExpires, and the final node could not enforce expiry. So the validated
// assertion is stashed and re-asserted verbatim.
func TestForwardedAuthoritySurvivesForTheNextHop(t *testing.T) {
	exp := time.Now().Add(time.Minute).Truncate(time.Second)
	inbound := &ForwardedAuthority{
		Kind:         AuthorityKindBadge,
		UserId:       "v1:identity:user:operator",
		PrimaryEmail: "operator@example.com",
		Role:         RoleReader,
		BadgeExpires: exp,
	}
	ctx := ContextWithForwardedAuthority(context.Background(), inbound)

	onward := ForwardedAuthorityFromContext(ctx)
	if onward == nil {
		t.Fatal("the inbound authority did not survive on the ctx; hop two would have to rebuild it")
	}
	if onward.Kind != AuthorityKindBadge {
		t.Errorf("kind degraded to %q on the second hop", onward.Kind)
	}
	if !onward.BadgeExpires.Equal(exp) {
		t.Errorf("badge expiry lost on the second hop: got %v want %v -- the final node could not gate an expired grant", onward.BadgeExpires, exp)
	}

	// A ctx that never came off a forward has nothing stashed, so the caller
	// knows to build one rather than silently forwarding nothing.
	if got := ForwardedAuthorityFromContext(context.Background()); got != nil {
		t.Errorf("a non-forwarded ctx produced an authority: %+v", got)
	}
}
