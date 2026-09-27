package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestCallOwner pins the rule every capability taking an ownerUserId resolves
// through (call_owner.go): the owner is the actor, and a different user is
// accepted only where the row write guard's escape set would accept a write
// to a row that is not the actor's.
func TestCallOwner(t *testing.T) {
	const userA, userB = "v1:identity:user:owner-a", "v1:identity:user:owner-b"
	person := func(id string) context.Context { return auth.ContextWithUserActor(context.Background(), id) }
	synthetic := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:x", Role: auth.RoleReader, Synthetic: true, Unranked: true,
	})
	clusterOwner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:operator", Role: auth.RoleOwner,
	})
	admin := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:admin", Role: auth.RoleAdmin,
	})
	cases := []struct {
		name      string
		ctx       context.Context
		requested string
		want      string
		refused   bool
	}{
		{"the caller's own id", person(userA), userA, userA, false},
		{"absent resolves to the caller", person(userA), "", userA, false},
		{"the caller's own id, bare", person(userA), "owner-a", "owner-a", false},
		{"a bare caller naming their canonical id", person("owner-a"), userA, userA, false},
		{"another user", person(userA), userB, "", true},
		{"the caller's short id under another concept", person(userA), "v1:agents:agent:owner-a", "", true},
		{"no actor at all", context.Background(), userA, "", true},
		{"no actor and no owner", context.Background(), "", "", false},
		{"an anonymous actor", auth.ContextWithAnonymousActor(context.Background()), userA, "", true},
		{"an anonymous actor names nobody when absent", auth.ContextWithAnonymousActor(context.Background()), "", "", false},
		{"a connector", auth.ContextWithConnectorActor(context.Background(), "shopify"), userA, "", true},
		{"an automation's actor at client origin", synthetic, userB, "", true},
		{"an automation's actor names nobody when absent", synthetic, "", "", false},
		{"internal origin", auth.ContextWithInternalOrigin(synthetic), userB, userB, false},
		{"internal origin under a person", auth.ContextWithInternalOrigin(person(userA)), userB, userB, false},
		{"a cluster owner", clusterOwner, userB, userB, false},
		// admin is not in the write guard's escape set (memql#3174), so it is
		// not here either.
		{"an admin", admin, userB, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CallOwner(tc.ctx, "op", tc.requested)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), OwnerNotCallerCode) {
					t.Fatalf("CallOwner(%q) = %q, %v; want the %s refusal", tc.requested, got, err, OwnerNotCallerCode)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("CallOwner(%q) = %q, %v; want %q", tc.requested, got, err, tc.want)
			}
		})
	}
}
