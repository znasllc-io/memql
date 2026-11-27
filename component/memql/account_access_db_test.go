package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// One isolated graph is necessary for a cluster-wide single-owner invariant.
// Other engine tests intentionally seed unrelated hypothetical clusters into
// the shared test database. This schema contains only this test's records.
func hierarchyTestEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, other, _, _, _ := livenessEngines(t)
	t.Cleanup(eng.cache.close)
	t.Cleanup(other.cache.close)
	return eng
}

func hierarchyCall(t *testing.T, e *MemQLEngine, user string, role auth.Role, account string, change map[string]any) (map[string]any, error) {
	t.Helper()
	args := map[string]any{"accountId": account}
	name := "accountAccess"
	if change != nil {
		name = "accountAccessUpdate"
		for key, value := range change {
			args[key] = value
		}
	}
	parts := []string{}
	for key, value := range args {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		parts = append(parts, key+": "+string(raw))
	}
	res, err := e.Execute(rankActorCtx(user, role), fmt.Sprintf("builtin %s(%s)", name, strings.Join(parts, ", ")))
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(res)
	require.Len(t, rows, 1)
	if payload, ok := rows[0]["payload"].(map[string]any); ok {
		return payload, nil
	}
	return rows[0], nil
}

func hierarchyFixture(t *testing.T) (*MemQLEngine, string, string) {
	t.Helper()
	e := hierarchyTestEngine(t)
	seedPrincipal(t, e, "primary-owner", auth.RoleOwner)
	for _, uid := range []string{"client-owner", "developer-a", "developer-b", "member", "stranger"} {
		seedPrincipal(t, e, uid, auth.Role("viewer"))
	}
	seedAccountOwnedBy(t, e, "primary-owner", auth.RoleOwner, "client", "Client")
	seedAccountOwnedBy(t, e, "primary-owner", auth.RoleOwner, "other", "Other")
	seedGroup(t, e, "acct-client", "Client", "account", "client")
	seedGroup(t, e, "acct-other", "Other", "account", "other")
	seedMembership(t, e, "acct-client", "primary-owner", "active")
	state, err := hierarchyCall(t, e, "primary-owner", auth.RoleOwner, "client", nil)
	require.NoError(t, err)
	change := func(args map[string]any) {
		args["revision"] = state["revision"]
		state, err = hierarchyCall(t, e, "primary-owner", auth.RoleOwner, "client", args)
		require.NoError(t, err)
	}
	change(map[string]any{"operation": "members", "userIds": []string{"client-owner", "developer-a", "developer-b", "member"}, "role": "viewer"})
	change(map[string]any{"operation": "apps", "apps": []string{"app:deployables", "app:email"}})
	change(map[string]any{"operation": "transfer", "userId": "client-owner"})
	return e, "client", fmt.Sprint(state["revision"])
}

func TestAccountHierarchyTransactional(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	owner := func(args map[string]any) (map[string]any, error) {
		args["revision"] = revision
		next, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, args)
		if err == nil {
			revision = fmt.Sprint(next["revision"])
		}
		return next, err
	}
	_, err := owner(map[string]any{"operation": "members", "userIds": []string{"developer-a", "developer-b"}, "role": "developer"})
	require.NoError(t, err)
	state, err := hierarchyCall(t, e, "developer-a", auth.Role("viewer"), account, nil)
	require.NoError(t, err)
	require.Equal(t, "developer", state["actorRole"])
	peerRow, _, err := authorityRow(context.Background(), e.database(), conceptIdentityGroupMembership, "acct-client-developer-b")
	require.NoError(t, err)
	peerPayload := accountRowPayload(peerRow)
	peerPayload["status"] = "removed"
	peerRow.Payload, _ = json.Marshal(peerPayload)
	peerRow.CreatedAt = time.Now().UTC().Add(time.Second)
	require.ErrorContains(t, newBunStore(e.database()).InsertMemoryNode(rankActorCtx("developer-a", auth.RoleDeveloper), &peerRow), "account_access_managed", "a raw write must not bypass peer management rules")
	_, err = hierarchyCall(t, e, "developer-a", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "members", "userIds": []string{"developer-b"}, "role": "viewer"})
	require.ErrorContains(t, err, "account_member_refused")
	_, err = owner(map[string]any{"operation": "members", "userIds": []string{"member"}, "role": "owner"})
	require.ErrorContains(t, err, "account_role_invalid")
	_, err = owner(map[string]any{"operation": "members", "userIds": []string{"client-owner"}, "remove": true})
	require.ErrorContains(t, err, "account_member_refused")
	_, err = owner(map[string]any{"operation": "apps", "apps": []string{"app:deployables"}})
	require.ErrorContains(t, err, "account_apps_refused")
	_, err = owner(map[string]any{"operation": "members", "userIds": []string{"member"}, "role": "member", "apps": []string{"app:files"}})
	require.ErrorContains(t, err, "account_apps_refused")
	_, err = owner(map[string]any{"operation": "members", "userIds": []string{"member", "stranger"}, "role": "member"})
	require.ErrorContains(t, err, "account_member_refused")
	row, found, err := authorityRow(context.Background(), e.database(), conceptIdentityGroupMembership, "acct-client-member")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "viewer", accountRowPayload(row)["organizationRole"], "failed batch must roll back its first member")
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), "other", nil)
	require.ErrorContains(t, err, "account_access_refused")
	_, err = owner(map[string]any{"operation": "members", "userIds": []string{"member"}, "role": "member", "apps": []string{"app:deployables"}})
	require.NoError(t, err)
	memberCtx := contextWithAccountScopeMemo(rankActorCtx("member", auth.Role("viewer")), e)
	require.True(t, e.OrganizationCapable(memberCtx, account, auth.VerbRead, "app:deployables"))
	require.False(t, e.OrganizationCapable(memberCtx, account, auth.VerbRead, "app:email"))
	require.False(t, e.OrganizationCapable(memberCtx, "other", auth.VerbRead, "app:deployables"))
	// Current ceiling is consulted on a fresh request, even if a subordinate
	// member still carries the app that was just removed.
	_, err = hierarchyCall(t, e, "primary-owner", auth.RoleOwner, account, map[string]any{"revision": revision, "operation": "apps", "apps": []string{}})
	require.NoError(t, err)
	require.False(t, e.OrganizationCapable(contextWithAccountScopeMemo(rankActorCtx("member", auth.Role("viewer")), e), account, auth.VerbRead, "app:deployables"))
}

func TestAccountHierarchyOwnershipRace(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, uid := range []string{"developer-a", "developer-b"} {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			_, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "transfer", "userId": uid})
			outcomes <- err
		}(uid)
	}
	wg.Wait()
	close(outcomes)
	succeeded, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			succeeded++
		} else if strings.Contains(err.Error(), "account_access_changed") || strings.Contains(err.Error(), "account_transfer_refused") {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, conflicts)
	state, err := hierarchyCall(t, e, "primary-owner", auth.RoleOwner, account, nil)
	require.NoError(t, err)
	owners := 0
	for _, raw := range state["people"].([]any) {
		if raw.(map[string]any)["role"] == "owner" {
			owners++
		}
	}
	require.Equal(t, 1, owners)
	// The ordinary writer cannot restore the former owner behind the transfer.
	row, _, err := authorityRow(context.Background(), e.database(), conceptAccount, account)
	require.NoError(t, err)
	payload := accountRowPayload(row)
	payload["ownerUserId"] = "v1:identity:user:client-owner"
	row.Payload, _ = json.Marshal(payload)
	row.CreatedAt = row.CreatedAt.Add(time.Microsecond)
	err = newBunStore(e.database()).InsertMemoryNode(context.Background(), &row)
	require.ErrorContains(t, err, "account_owner_transfer_required")
}

func TestAccountHierarchyDeveloperPrivacy(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	_, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "members", "userIds": []string{"developer-a", "developer-b"}, "role": "developer"})
	require.NoError(t, err)
	seedSiteFor(t, e, "private-a", "developer-a", account)
	seedSiteFor(t, e, "private-b", "developer-b", account)
	seen := sitesVisibleTo(t, e, "developer-a", auth.Role("viewer"))
	require.True(t, seen["private-a"])
	require.False(t, seen["private-b"], "common account membership cannot expose peer developer data")
	ctx := contextWithAccountScopeMemo(rankActorCtx("developer-a", auth.Role("viewer")), e)
	ctx = contextWithRankScopeMemo(ctx, e)
	payload, _ := json.Marshal(map[string]any{"ownerUserId": "developer-b", "accountId": account})
	require.Equal(t, rowAuthzDeny, rowAuthzAdmitsMode(ctx, conceptPlatformSite, "private-b", payload, false))
	require.Equal(t, rowAuthzDeny, rowAuthzAdmitsMode(ctx, conceptPlatformSite, "private-b", payload, true))
	ownerSeen := sitesVisibleTo(t, e, "client-owner", auth.Role("viewer"))
	require.True(t, ownerSeen["private-b"])
}

func TestAccountHierarchyPrimaryTransferAndOrdinaryWriters(t *testing.T) {
	e := hierarchyTestEngine(t)
	seedPrincipal(t, e, "primary-owner", auth.RoleOwner)
	seedPrincipal(t, e, "successor", auth.Role("viewer"))
	seedAccountOwnedBy(t, e, "primary-owner", auth.RoleOwner, "self", "Primary")
	seedGroup(t, e, "acct-self", "Primary", "account", "self")
	seedMembership(t, e, "acct-self", "successor", "active")
	state, err := hierarchyCall(t, e, "primary-owner", auth.RoleOwner, "self", nil)
	require.NoError(t, err)
	state, err = hierarchyCall(t, e, "primary-owner", auth.RoleOwner, "self", map[string]any{"revision": state["revision"], "operation": "transfer", "userId": "successor"})
	require.NoError(t, err)
	require.Equal(t, "successor", state["ownerUserId"])
	owners, err := activePrimaryOwners(context.Background(), e.database())
	require.NoError(t, err)
	require.Equal(t, []string{"successor"}, owners)
	for _, uid := range []string{"primary-owner", "successor"} {
		row, _, err := authorityRow(context.Background(), e.database(), conceptIdentityUser, uid)
		require.NoError(t, err)
		epoch, _ := intWithOkFromAny(accountRowPayload(row)["revocationEpoch"])
		require.Equal(t, 1, epoch, "old JWT roles must be revoked as part of transfer")
		p := accountRowPayload(row)
		if uid == "primary-owner" {
			p["role"] = "owner"
		} else {
			p["active"] = false
		}
		row.Payload, _ = json.Marshal(p)
		row.CreatedAt = time.Now().UTC().Add(time.Second)
		err = newBunStore(e.database()).InsertMemoryNode(context.Background(), &row)
		require.Error(t, err, "ordinary writes cannot add or remove the sole owner")
	}
	_, err = hierarchyCall(t, e, "primary-owner", auth.RoleOwner, "self", map[string]any{"revision": state["revision"], "operation": "transfer", "userId": "primary-owner"})
	require.ErrorContains(t, err, "account_transfer_refused", "a stale owner claim must not authorize another transfer")
}

func TestAccountHierarchyGroupsAndLeaving(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	_, err := hierarchyCall(t, e, "stranger", auth.Role("viewer"), account, map[string]any{"operation": "leave", "groupId": "acct-client"})
	require.ErrorContains(t, err, "account_membership_required")
	change := func(args map[string]any) map[string]any {
		args["revision"] = revision
		state, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, args)
		require.NoError(t, err)
		revision = fmt.Sprint(state["revision"])
		return state
	}
	change(map[string]any{"operation": "members", "userIds": []string{"member"}, "role": "member"})
	// An older groupMemberAdd caller omits role. Re-adding must not demote.
	change(map[string]any{"operation": "members", "userIds": []string{"member"}})
	row, _, err := authorityRow(context.Background(), e.database(), conceptIdentityGroupMembership, "acct-client-member")
	require.NoError(t, err)
	require.Equal(t, "member", accountRowPayload(row)["organizationRole"])
	state := change(map[string]any{"operation": "createGroup", "name": "Reviewers", "description": "Review team"})
	groupID := fmt.Sprint(state["groupId"])
	change(map[string]any{"operation": "groupMembers", "groupId": groupID, "userIds": []string{"member", "developer-a"}})
	change(map[string]any{"operation": "groupApps", "groupId": groupID, "apps": []string{"app:deployables"}})
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "groupApps", "groupId": groupID, "apps": []string{"app:settings"}})
	require.ErrorContains(t, err, "account_apps_invalid")
	_, err = hierarchyCall(t, e, "member", auth.Role("viewer"), account, map[string]any{"operation": "leave", "groupId": "acct-client"})
	require.NoError(t, err)
	for _, gid := range []string{"acct-client", groupID} {
		row, _, err = authorityRow(context.Background(), e.database(), conceptIdentityGroupMembership, gid+"-member")
		require.NoError(t, err)
		require.Equal(t, "removed", accountRowPayload(row)["status"])
	}
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"operation": "leave", "groupId": "acct-client"})
	require.ErrorContains(t, err, "account_owner_required")
}

func TestAccountHierarchyFirstOwnerRace(t *testing.T) {
	e := hierarchyTestEngine(t)
	for _, uid := range []string{"first", "second"} {
		seedPrincipal(t, e, uid, auth.Role("viewer"))
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, uid := range []string{"first", "second"} {
		row, _, err := authorityRow(context.Background(), e.database(), conceptIdentityUser, uid)
		require.NoError(t, err)
		p := accountRowPayload(row)
		p["role"] = "owner"
		row.Payload, err = json.Marshal(p)
		require.NoError(t, err)
		row.CreatedAt = time.Now().UTC().Add(time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- newBunStore(e.database()).InsertMemoryNode(context.Background(), &row)
		}()
	}
	wg.Wait()
	close(results)
	accepted, refused := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			require.ErrorContains(t, err, "account_single_owner")
			refused++
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, refused)
	owners, err := activePrimaryOwners(context.Background(), e.database())
	require.NoError(t, err)
	require.Len(t, owners, 1)
}

func TestAccountHierarchyPrivacyRefreshAndAccountOwnership(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	state, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "members", "userIds": []string{"developer-a", "developer-b"}, "role": "member", "apps": []string{"app:deployables"}})
	require.NoError(t, err)
	revision = fmt.Sprint(state["revision"])
	seedSiteFor(t, e, "peer-work", "developer-b", account)
	require.True(t, sitesVisibleTo(t, e, "developer-a", auth.Role("viewer"))["peer-work"])
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "members", "userIds": []string{"developer-a", "developer-b"}, "role": "developer"})
	require.NoError(t, err)
	// Execute the same query again: neither its cached plan nor its cached
	// rows can preserve access after a peer becomes a developer.
	require.False(t, sitesVisibleTo(t, e, "developer-a", auth.Role("viewer"))["peer-work"])
	seedAccountOwnedBy(t, e, "developer-a", auth.Role("viewer"), "owned-client", "Owned client")
	seedGroup(t, e, "acct-owned-client", "Owned client", "account", "owned-client")
	seedMembership(t, e, "acct-owned-client", "developer-a", "active")
	seedSiteFor(t, e, "owned-client-work", "developer-b", "owned-client")
	seen := sitesVisibleTo(t, e, "developer-a", auth.Role("viewer"))
	require.False(t, seen["peer-work"])
	require.True(t, seen["owned-client-work"], "ownership of another account must retain its oversight")
	ctx := contextWithRankScopeMemo(contextWithAccountScopeMemo(rankActorCtx("developer-a", auth.Role("viewer")), e), e)
	payload, _ := json.Marshal(map[string]any{"ownerUserId": "developer-b", "accountId": "owned-client"})
	require.Equal(t, rowAuthzAdmit, rowAuthzAdmitsMode(ctx, conceptPlatformSite, "owned-client-work", payload, false))
}

func TestAccountHierarchyClusterDeveloperPrivacy(t *testing.T) {
	e, account, _ := hierarchyFixture(t)
	seedPrincipal(t, e, "developer-a", auth.RoleDeveloper)
	seedPrincipal(t, e, "developer-b", auth.RoleDeveloper)
	seedSiteFor(t, e, "global-private-a", "developer-a", account)
	seedSiteFor(t, e, "global-private-b", "developer-b", account)
	for _, actor := range []string{"developer-a", "developer-b"} {
		seen := sitesVisibleTo(t, e, actor, auth.RoleDeveloper)
		require.Equal(t, actor == "developer-a", seen["global-private-a"])
		require.Equal(t, actor == "developer-b", seen["global-private-b"])
	}
	borrowed := auth.ContextWithUserActor(context.Background(), "developer-a")
	borrowed = contextWithRankScopeMemo(contextWithAccountScopeMemo(borrowed, e), e)
	payload, _ := json.Marshal(map[string]any{"ownerUserId": "developer-b", "accountId": account})
	require.Equal(t, rowAuthzDeny, rowAuthzAdmitsMode(borrowed, conceptPlatformSite, "global-private-b", payload, false))
}

func TestAccountHierarchyInvitationProjectionAndRevocation(t *testing.T) {
	e, account, revision := hierarchyFixture(t)
	for _, fixture := range []struct{ id, group string }{{"local-invite", "acct-client"}, {"foreign-invite", "acct-other"}} {
		seedAuthorityFixture(t, e, "v1:identity:invitation", fixture.id, map[string]any{
			"active": true, "kind": "user", "status": "pending", "inviteeRole": "viewer",
			"expiresAt":    time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			"inviteeEmail": "invitee@example.test", "inviterId": "client-owner", "groupIds": []string{fixture.group}, "tokenHash": "never-return-this",
		})
	}
	state, err := hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, nil)
	require.NoError(t, err)
	invites := state["invitations"].([]any)
	require.Len(t, invites, 1)
	encoded, err := json.Marshal(invites)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "never-return-this")
	require.NotContains(t, string(encoded), "foreign-invite")
	_, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "revokeInvitation", "invitationId": "foreign-invite"})
	require.Error(t, err)
	state, err = hierarchyCall(t, e, "client-owner", auth.Role("viewer"), account, map[string]any{"revision": revision, "operation": "revokeInvitation", "invitationId": "local-invite"})
	require.NoError(t, err)
	require.Empty(t, state["invitations"])
	row, _, err := authorityRow(context.Background(), e.database(), "v1:identity:invitation", "local-invite")
	require.NoError(t, err)
	require.Equal(t, false, accountRowPayload(row)["active"])
}
