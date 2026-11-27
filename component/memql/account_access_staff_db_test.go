package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// THE STANDING STAFF RULE (groups-and-grants design, D6): developer rank and
// above are standing members of every account-kind group. The account
// hierarchy (2026-11-26) answers client authority from the account's owner and
// its membership rows only, so a cluster developer who was never placed in a
// client's group could not open that client's people at all
// ("account_access_refused"). These tests hold the rule on the hierarchy: a
// cluster developer manages every client account as its Developer, below the
// client's owner and never over a peer, and a cluster admin is not staff.
func TestAccountHierarchyStaffDeveloperManagesEveryClientAccount(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	seedPrincipal(t, e, "staff-dev", auth.RoleDeveloper)
	seedPrincipal(t, e, "cluster-admin", auth.RoleAdmin)

	state, err := hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, account, nil)
	require.NoError(t, err, "a cluster developer is standing staff of every client account")
	require.Equal(t, "developer", state["actorRole"])

	_, err = hierarchyCall(t, e, "cluster-admin", auth.RoleAdmin, account, nil)
	require.ErrorContains(t, err, "account_access_refused", "the standing staff rule starts at developer rank")

	staff := func(args map[string]any) (map[string]any, error) {
		args["revision"] = revision
		next, err := hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, account, args)
		if err == nil {
			revision = fmt.Sprint(next["revision"])
		}
		return next, err
	}
	// People below Developer: a member's role and apps within the ceiling.
	_, err = staff(map[string]any{"operation": "members", "userIds": []string{"member"}, "role": "member", "apps": []string{"app:deployables"}})
	require.NoError(t, err)
	// Somebody who is not in the account yet, as the cluster owner may.
	_, err = staff(map[string]any{"operation": "members", "userIds": []string{"stranger"}, "role": "viewer"})
	require.NoError(t, err)
	// Never the client's owner.
	_, err = staff(map[string]any{"operation": "members", "userIds": []string{"client-owner"}, "role": "viewer"})
	require.ErrorContains(t, err, "account_member_refused")
	// Never a peer: a client Developer is not below a staff Developer.
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "members", "userIds": []string{"developer-a"}, "role": "developer"})
	require.NoError(t, err)
	state, err = hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, account, nil)
	require.NoError(t, err)
	revision = fmt.Sprint(state["revision"])
	_, err = staff(map[string]any{"operation": "members", "userIds": []string{"developer-a"}, "role": "viewer"})
	require.ErrorContains(t, err, "account_member_refused")
	// Ownership stays the owner's to transfer.
	_, err = staff(map[string]any{"operation": "transfer", "userId": "member"})
	require.ErrorContains(t, err, "account_transfer_refused")
}

// D6 is about client accounts. On the primary account a membership change
// also rewrites the person's cluster role, so placing somebody who is not yet
// in it stays the cluster owner's, exactly as before the staff rule.
func TestAccountHierarchyStaffRuleStaysOffThePrimaryAccount(t *testing.T) {
	e, _, _ := hierarchyFixture(t)
	seedPrincipal(t, e, "staff-dev", auth.RoleDeveloper)
	seedAccountOwnedBy(t, e, "primary-owner", auth.RoleOwner, "self", "Primary")
	seedGroup(t, e, "acct-self", "Primary", "account", "self")
	state, err := hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, "self", nil)
	require.NoError(t, err)
	for _, c := range state["candidates"].([]any) {
		require.NotEqual(t, "client-owner", c.(map[string]any)["id"], "staff are offered nobody outside the primary account")
	}
	_, err = hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, "self", map[string]any{"revision": fmt.Sprint(state["revision"]), "operation": "members", "userIds": []string{"client-owner"}, "role": "admin"})
	require.ErrorContains(t, err, "account_member_refused")
	row, _, err := authorityRow(context.Background(), e.database(), conceptIdentityUser, "client-owner")
	require.NoError(t, err)
	require.Equal(t, "viewer", accountRowPayload(row)["role"], "a refused change must leave the cluster role alone")
}

// Staff already stand in every client account. Writing a membership row for a
// fellow staff developer would only misstate them (a "viewer" who manages as
// Developer) and act on a peer, so a staff developer neither sees nor places them.
func TestAccountHierarchyStaffDoesNotPlacePeerStaff(t *testing.T) {
	e, account, _ := hierarchyFixture(t)
	seedPrincipal(t, e, "staff-dev", auth.RoleDeveloper)
	seedPrincipal(t, e, "staff-dev-2", auth.RoleDeveloper)
	state, err := hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, account, nil)
	require.NoError(t, err)
	ids := map[any]bool{}
	for _, c := range state["candidates"].([]any) {
		ids[c.(map[string]any)["id"]] = true
	}
	require.True(t, ids["stranger"], "people outside the client are offered")
	require.False(t, ids["staff-dev-2"], "a peer staff developer is not offered")
	require.False(t, ids["staff-dev"], "nor the actor")
	_, err = hierarchyCall(t, e, "staff-dev", auth.RoleDeveloper, account, map[string]any{"revision": fmt.Sprint(state["revision"]), "operation": "members", "userIds": []string{"staff-dev-2"}, "role": "viewer"})
	require.ErrorContains(t, err, "account_member_refused")
}
