package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Organization membership, including peer-authored accounts and groups, is
// the row boundary. An owner-only filter would silently hide these records.
func TestAccountsAndGroupsBrowsePreserveOrganizationMembershipDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrants); auth.SetMembershipSource(oldMembers) })
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("identity-directory")}
	member, peer := f.person("member", true), f.person("peer", true)
	needle := f.prefix + " shared"
	contacts := map[string]string{}
	for n := 0; n < 26; n++ {
		account := fmt.Sprintf("%s-account-%02d", f.prefix, n)
		owner := peer
		if n == 25 {
			owner = member
		}
		id := conceptAccountsAccount + ":" + account
		contact := fmt.Sprintf("contact-%02d@%s.example.test", n, f.prefix)
		contacts[id] = contact
		f.insert(conceptAccountsAccount, id, map[string]any{"ownerUserId": owner, "name": needle, "status": "active", "domainStatus": "unverified", "primaryContactName": fmt.Sprintf("Contact %02d", n), "primaryContactEmail": contact})
		seedGroup(t, engine, "g-"+account, needle, "account", account)
		if n < 25 {
			seedMembership(t, engine, "g-"+account, BareShortId(member), "active")
		}
	}
	ctx := rankActorCtx(member, auth.RoleWriter)
	// The existing picker intentionally has no admin floor: current members
	// below that rank may read peer-authored organization contact details.
	legacy, err := engine.Execute(ctx, `query clientAccountsAll()`)
	require.NoError(t, err)
	legacyContacts := map[string]string{}
	for _, row := range MaterializeRows(legacy.OutputPayload()) {
		id := fmt.Sprint(row["id"])
		if _, belongs := contacts[id]; belongs {
			legacyContacts[id] = fmt.Sprint(row["primaryContactEmail"])
		}
	}
	require.Len(t, legacyContacts, 25)
	for _, name := range []string{"clientAccountsBrowse", "groupsBrowse"} {
		t.Run(name, func(t *testing.T) {
			cursor := ""
			seen := map[string]bool{}
			for page, size := range []int{10, 10, 5} {
				result, err := engine.Execute(ContextWithCursor(ctx, cursor), fmt.Sprintf("query %s(search:%q)", name, needle))
				require.NoError(t, err)
				rows := MaterializeRows(result.OutputPayload())
				require.Len(t, rows, size)
				for _, row := range rows {
					id := fmt.Sprint(row["id"])
					require.False(t, seen[id])
					seen[id] = true
					require.NotContains(t, id, "account-25")
					if name == "clientAccountsBrowse" {
						require.Equal(t, contacts[id], row["primaryContactEmail"])
						require.Equal(t, legacyContacts[id], row["primaryContactEmail"])
						require.NotEmpty(t, row["primaryContactName"])
					}
				}
				cursor = result.GetMeta().Cursor
				if page < 2 {
					require.NotEmpty(t, cursor)
				} else {
					require.Empty(t, cursor)
				}
			}
		})
	}
	// Neither a signed-in outsider nor an anonymous caller gains contact
	// details by using the new browse entry point instead of the old picker.
	outsider := f.person("outsider", true)
	for _, denied := range []context.Context{rankActorCtx(outsider, auth.RoleWriter), context.Background()} {
		old, oldErr := engine.Execute(denied, `query clientAccountsAll()`)
		browse, browseErr := engine.Execute(denied, fmt.Sprintf(`query clientAccountsBrowse(search:%q)`, needle))
		require.Equal(t, oldErr == nil, browseErr == nil, "browse must retain the original caller admission contract")
		if oldErr == nil {
			require.Empty(t, MaterializeRows(old.OutputPayload()))
			require.Empty(t, MaterializeRows(browse.OutputPayload()))
		}
	}
}

// This is the exact directive chain sent by queryPage around the existing
// authorized queries. The legacy unpaged contracts remain independently usable.
func TestExistingIdentityQueriesPageSearchAndKeepTheirRankFloorDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("identity-page")}
	caller := f.person("operator", true)
	ctx := rankActorCtx(caller, auth.RoleDeveloper)
	prefix := f.prefix + " needle%_"
	for n := 0; n < 25; n++ {
		name := fmt.Sprintf("%s %02d", prefix, n)
		f.insert(conceptIdentityUser, f.id(conceptIdentityUser, fmt.Sprintf("user-%02d", n)), map[string]any{"active": true, "displayName": name, "primaryEmail": fmt.Sprintf("%s-%02d@example.test", f.prefix, n), "role": "viewer"})
		f.insert("v1:identity:invitation", f.id("v1:identity:invitation", fmt.Sprintf("invite-%02d", n)), map[string]any{"active": true, "status": "pending", "kind": "user", "inviteeName": name, "inviteeEmail": fmt.Sprintf("%s-invite-%02d@example.test", f.prefix, n), "inviteeRole": "viewer", "groupIds": []string{"test-group"}, "expiresAt": "2099-01-01T00:00:00Z", "tokenHash": "hidden-token-hash"})
	}
	expired := f.id("v1:identity:invitation", "expired")
	f.insert("v1:identity:invitation", expired, map[string]any{"active": true, "status": "pending", "kind": "user", "inviteeName": prefix, "inviteeRole": "viewer", "groupIds": []string{"test-group"}, "expiresAt": "2020-01-01T00:00:00Z", "tokenHash": "hidden-token-hash"})
	for _, call := range []string{
		fmt.Sprintf(`paginate(query searchUsers(search:%q,role:"viewer",showDeactivated:false),10)`, prefix),
		fmt.Sprintf(`paginate(query pendingUserInvitations(search:%q,role:"viewer",groupId:"test-group",unexpired:true),10)`, prefix),
	} {
		cursor := ""
		seen := map[string]bool{}
		var previous string
		for page, size := range []int{10, 10, 5} {
			result, err := engine.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
			rows := MaterializeRows(result.OutputPayload())
			require.Len(t, rows, size)
			for _, row := range rows {
				id := fmt.Sprint(row["id"])
				require.False(t, seen[id])
				seen[id] = true
				require.NotEqual(t, expired, id)
				require.NotContains(t, row, "tokenHash")
				created := fmt.Sprint(row["createdAt"])
				if previous != "" {
					require.GreaterOrEqual(t, previous, created)
				}
				previous = created
			}
			cursor = result.GetMeta().Cursor
			if page < 2 {
				require.NotEmpty(t, cursor)
			} else {
				require.Empty(t, cursor)
			}
		}
		_, err := engine.Execute(rankActorCtx(caller, auth.RoleWriter), call)
		require.Error(t, err, "outer paging must not bypass the named query rank floor")
	}
	// The invitation picker did not previously exclude expired links. Its
	// omitted new options still keep that contract; only the directory opts in.
	legacy, err := engine.Execute(ctx, fmt.Sprintf(`query pendingUserInvitations(search:%q)`, prefix))
	require.NoError(t, err)
	require.Len(t, MaterializeRows(legacy.OutputPayload()), 26)
	// Legacy records may omit the optional active field. The UI treats those
	// as active unless suspended, and the trait spelling must preserve that.
	unset := f.id(conceptIdentityUser, "unset-active")
	unsetName := f.prefix + " unset active"
	f.insert(conceptIdentityUser, unset, map[string]any{"displayName": unsetName, "primaryEmail": f.prefix + "-unset@example.test", "role": "viewer"})
	f.insert(conceptIdentityUser, f.id(conceptIdentityUser, "inactive"), map[string]any{"displayName": unsetName, "primaryEmail": f.prefix + "-inactive@example.test", "role": "viewer", "active": false})
	for _, state := range []string{"", "active"} {
		result, err := engine.Execute(ctx, fmt.Sprintf(`paginate(query searchUsers(search:%q,showDeactivated:false,state:%q),10)`, unsetName, state))
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, 1)
		require.Equal(t, unset, rows[0]["id"])
	}
}

func TestPeopleBrowseSearchesFallbackFullNamesBeforePagingDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("people-full-name")}
	caller := f.person("operator", true)
	plain := f.id(conceptIdentityUser, "full-name")
	literal := f.id(conceptIdentityUser, "literal-name")
	for _, person := range []struct{ id, first, last string }{
		{plain, "Jane", "Doe"}, {literal, "Jane%_", "D'oe"},
	} {
		f.insert(conceptIdentityUser, person.id, map[string]any{"displayName": "", "firstName": person.first, "lastName": person.last, "active": true, "role": "viewer"})
	}
	for i := 0; i < 25; i++ {
		f.person(fmt.Sprintf("newer-%02d", i), true)
	}
	ctx := rankActorCtx(caller, auth.RoleDeveloper)
	for _, tc := range []struct{ search, want string }{
		{"jAnE dOe", plain}, {"ne%_ d'O", literal},
	} {
		call := fmt.Sprintf(`paginate(query searchUsers(search:%q,showDeactivated:false),10)`, tc.search)
		result, err := engine.Execute(ctx, call)
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, 1, "the full name shown by the People view must be searchable even beyond the initial page")
		require.Equal(t, tc.want, rows[0]["id"])
		_, err = engine.Execute(rankActorCtx(caller, auth.RoleWriter), call)
		require.Error(t, err, "full-name search must retain the existing developer floor")
	}
}
