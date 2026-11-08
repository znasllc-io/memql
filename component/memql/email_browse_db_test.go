package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Real organization memberships admit peer-authored rows before pagination;
// a matching name in another organization must not consume a page position.
func TestEmailBrowsePagesSearchAndOrganizationScopeDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrants); auth.SetMembershipSource(oldMembers) })
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("email-browse")}
	member, peer := f.person("member", true), f.person("peer", true)
	account, other := "acme-"+f.prefix, "other-"+f.prefix
	for _, id := range []string{account, other} {
		f.insert(conceptAccountsAccount, conceptAccountsAccount+":"+id, map[string]any{"name": id, "status": "active", "domainStatus": "unverified"})
	}
	seedGroup(t, engine, "g-"+account, account, "account", account)
	seedMembership(t, engine, "g-"+account, BareShortId(member), "active")
	ctx := rankActorCtx(member, auth.RoleWriter)
	for _, resource := range []struct{ concept, query string }{{"campaign", "campaignsBrowse"}, {"audience", "audiencesBrowse"}, {"template", "templatesBrowse"}, {"journey", "journeysBrowse"}, {"emailRule", "emailRulesBrowse"}} {
		t.Run(resource.query, func(t *testing.T) {
			concept := "v1:campaigns:" + resource.concept
			for n := 0; n < 25; n++ {
				name := fmt.Sprintf("Recent %02d", n)
				if n < 12 {
					name = fmt.Sprintf("Earlier NEEDLE%%_ %02d", n)
				}
				payload := map[string]any{"ownerUserId": peer, "accountId": account, "name": name, "description": "Useful description", "status": "active", "confirmation": "required", "trigger": map[string]any{"kind": "joined"}, "subject": "Subject", "textBody": strings.Repeat("private body", 2048), "document": map[string]any{"private": "document"}}
				f.insert(concept, f.id(concept, fmt.Sprintf("item-%02d", n)), payload)
			}
			// Even an author cannot use the owner fallback to cross an organization's
			// explicit boundary; peer-authored Acme records remain readable.
			f.insert(concept, f.id(concept, "foreign"), map[string]any{"ownerUserId": member, "accountId": other, "name": "Earlier NEEDLE%_ foreign", "status": "active"})
			walk := func(call string, sizes []int) {
				cursor := ""
				seen := map[string]bool{}
				for n, size := range sizes {
					result, err := engine.Execute(ContextWithCursor(ctx, cursor), call)
					require.NoError(t, err)
					rows := MaterializeRows(result.OutputPayload())
					require.Len(t, rows, size)
					payload, err := json.Marshal(rows)
					require.NoError(t, err)
					require.Less(t, len(payload), 16*1024)
					for _, row := range rows {
						id := fmt.Sprint(row["id"])
						require.False(t, seen[id])
						seen[id] = true
						require.NotContains(t, id, "foreign")
						require.Equal(t, account, BareShortId(fmt.Sprint(row["accountId"])))
						require.NotContains(t, row, "textBody")
						require.NotContains(t, row, "document")
					}
					cursor = result.GetMeta().Cursor
					if n < len(sizes)-1 {
						require.NotEmpty(t, cursor)
					} else {
						require.Empty(t, cursor)
					}
				}
			}
			walk("query "+resource.query+"()", []int{10, 10, 5})
			walk("query "+resource.query+`(search: "needle%_")`, []int{10, 2})
		})
	}
}

// The directory reducer and run-label lookup retain these exact named reads.
// Membership, not creator identity, determines organization-owned results.
func TestPlatformDirectorySourcesOrganizationScopeDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrants); auth.SetMembershipSource(oldMembers) })
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("directory-scope")}
	member, peer := f.person("member", true), f.person("peer", true)
	account, other := "acme-"+f.prefix, "other-"+f.prefix
	for _, id := range []string{account, other} {
		f.insert(conceptAccountsAccount, conceptAccountsAccount+":"+id, map[string]any{"name": id, "status": "active", "domainStatus": "unverified"})
	}
	seedGroup(t, engine, "g-"+account, account, "account", account)
	seedMembership(t, engine, "g-"+account, BareShortId(member), "active")
	ctx := rankActorCtx(member, auth.RoleWriter)
	for _, resource := range []struct{ concept, query, status string }{{"site", "deployableDirectorySites", "live"}, {"package", "deployableDirectoryPackages", "active"}, {"packageDeployment", "deployableDirectoryPending", "awaiting_confirm"}} {
		concept := "v1:platform:" + resource.concept
		ids := []string{}
		for n := 0; n < 12; n++ {
			id := f.id(concept, fmt.Sprintf("peer-%02d", n))
			ids = append(ids, id)
			f.insert(concept, id, map[string]any{"ownerUserId": peer, "accountId": account, "status": resource.status, "name": "Shared directory item", "title": "Shared directory item", "hostname": "shared.example.test", "sourceKind": "repo", "repoUrl": "https://github.com/acme/project"})
		}
		foreign := f.id(concept, "foreign")
		ids = append(ids, foreign)
		f.insert(concept, foreign, map[string]any{"ownerUserId": member, "accountId": other, "status": resource.status, "name": "Foreign directory item"})
		calls := []string{"query " + resource.query + `(asOf: "2099-01-01T00:00:00Z")`}
		rawIDs, _ := json.Marshal(ids)
		if resource.concept == "site" {
			calls = append(calls, fmt.Sprintf("query deployableDirectorySitesByIds(ids: %s)", rawIDs))
		}
		if resource.concept == "package" {
			calls = append(calls, fmt.Sprintf("query packageNamesByIds(ids: %s)", rawIDs))
		}
		for _, call := range calls {
			t.Run(call, func(t *testing.T) {
				result, err := engine.Execute(ctx, call)
				require.NoError(t, err)
				rows := MaterializeRows(result.OutputPayload())
				require.Len(t, rows, 12)
				for _, row := range rows {
					require.NotContains(t, fmt.Sprint(row["id"]), "foreign")
				}
			})
		}
	}
}
