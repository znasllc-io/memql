package auth

import (
	"context"
	"strings"
	"testing"
)

func TestConnectorActorCarriesItsNameAndTheConnectorRole(t *testing.T) {
	ac := ConnectorActor("shopify")
	if ac == nil {
		t.Fatal("ConnectorActor(\"shopify\") returned nil")
	}
	if ac.Role != RoleConnector {
		t.Errorf("Role = %q, want %q", ac.Role, RoleConnector)
	}
	name, ok := ac.ConnectorNameValue()
	if !ok || name != "shopify" {
		t.Errorf("ConnectorNameValue() = (%q, %v), want (\"shopify\", true)", name, ok)
	}
	if !ac.IsConnector() {
		t.Error("IsConnector() = false on a connector actor")
	}
	if !strings.HasPrefix(ac.UserId, connectorUserIdPrefix) {
		t.Errorf("UserId = %q, want the %q prefix -- a row a connector wrote has to say so in createdBy", ac.UserId, connectorUserIdPrefix)
	}
	if ac.UserId == connectorUserIdPrefix {
		t.Error("UserId is the bare prefix -- surfaces that read a blank identity as \"no actor\" need a non-empty one")
	}
}

// A caller that cannot say which connector it is gets no actor at all,
// rather than an unnamed writer row admission would have to guess about.
func TestConnectorActorRefusesABlankName(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if ac := ConnectorActor(name); ac != nil {
			t.Errorf("ConnectorActor(%q) = %+v, want nil", name, ac)
		}
	}
	ctx := context.Background()
	if got := ContextWithConnectorActor(ctx, "  "); got != ctx {
		t.Error("ContextWithConnectorActor with a blank name changed the context -- the work would then run as whoever the inbound caller was")
	}
}

func TestConnectorFromContextRoundTrips(t *testing.T) {
	ctx := ContextWithConnectorActor(context.Background(), "shopify")
	name, ok := ConnectorFromContext(ctx)
	if !ok || name != "shopify" {
		t.Fatalf("ConnectorFromContext = (%q, %v), want (\"shopify\", true)", name, ok)
	}
	if _, ok := ConnectorFromContext(context.Background()); ok {
		t.Error("ConnectorFromContext found a connector on a bare context")
	}
}

// Both halves have to agree. A half-built actor denies, because the
// connector rule is the only thing that would have admitted it.
func TestAMalformedConnectorActorIsNotAConnector(t *testing.T) {
	cases := []struct {
		name string
		ac   *AccessContext
	}{
		{"name without the role", &AccessContext{UserId: "u1", Role: RoleWriter, ConnectorName: "shopify"}},
		{"role without the name", &AccessContext{UserId: "u1", Role: RoleConnector}},
		{"nil actor", nil},
		{"an ordinary user", &AccessContext{UserId: "u1", Role: RoleOwner}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ac.IsConnector() {
				t.Errorf("%+v reads as a connector -- a half-built actor must deny, not admit", tc.ac)
			}
		})
	}
}

// The connector role is not a role a user may hold, and it is not
// privileged. Its power comes from the targeted row-admission rule keyed
// on its name; leaked into a normal request it must grant less than a
// reader, never more.
func TestRoleConnectorIsNotAUserRoleAndIsNotPrivileged(t *testing.T) {
	for _, r := range ValidRoles() {
		if r == RoleConnector {
			t.Fatal("RoleConnector appears in ValidRoles() -- a user could then be assigned it, and an identity issued with it")
		}
	}
	if got, want := RoleLevel(RoleConnector), RoleLevel(RoleReader); got != want {
		t.Errorf("RoleLevel(RoleConnector) = %d, want %d (the least privileged tier)", got, want)
	}
	if ConnectorActor("shopify").IsClusterOwner() {
		t.Error("a connector actor reads as a cluster owner")
	}
}
