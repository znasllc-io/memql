package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The authorized business role has only these three role capabilities. App
// grants and membership below are real rows, with the production grant shape.
// Fixing the catalog here isolates the authority under test from local seeds.
type fyloBusinessCatalog struct{ organizationGroupsWireCatalog }

func (c fyloBusinessCatalog) Rank(slug string) (int, bool) {
	if slug == c.role {
		return 20, true
	}
	return c.organizationGroupsWireCatalog.Rank(slug)
}
func (c fyloBusinessCatalog) Holds(slug, verb, resource string) bool {
	return slug == "owner" || slug == c.role && resource == auth.ResourceData &&
		(verb == auth.VerbRead || verb == auth.VerbCreate || verb == auth.VerbUpdate)
}
func (c fyloBusinessCatalog) Grants(slug string) []auth.VerbResource {
	if slug != c.role {
		return nil
	}
	return []auth.VerbResource{{Verb: auth.VerbRead, Resource: auth.ResourceData}, {Verb: auth.VerbCreate, Resource: auth.ResourceData}, {Verb: auth.VerbUpdate, Resource: auth.ResourceData}}
}

func TestFyloBusinessRoleCannotCrossAccountsOrChangeDeploymentControl(t *testing.T) {
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	eng.InstallGrantResolution()
	suffix := fmt.Sprintf("business-role-%d", time.Now().UnixNano())
	user, peer, account, foreign, role := suffix+"-user", suffix+"-peer", suffix+"-fylo", suffix+"-foreign", suffix+"-moderator"
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("business-role-fixture")))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: "business-role-fixture"})
	quote := langparser.QuoteString
	ids := map[string]bool{}
	raw := func(kind, id string, payload map[string]any) string {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		ids[kind+":"+id] = true
		return fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, quote(kind), quote(id), body)
	}
	put := func(kind, id string, payload map[string]any) {
		t.Helper()
		_, err := eng.Execute(seed, raw(kind, id, payload))
		require.NoError(t, err)
	}
	seedCall := func(query string) { t.Helper(); _, err := eng.Execute(seed, query); require.NoError(t, err) }
	t.Cleanup(func() {
		for id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	for _, person := range []string{user, peer} {
		put("v1:identity:user", person, map[string]any{"displayName": person, "primaryEmail": person + "@example.test", "role": "reader", "active": true})
	}
	for _, a := range []string{account, foreign} {
		put("v1:accounts:account", a, map[string]any{"name": a, "status": "active", "domainStatus": "unverified"})
		owner := peer
		if a == foreign {
			owner = user // Ownership must not override the scoped role.
		}
		put("v1:campaigns:audience", "aud-"+a, map[string]any{"ownerUserId": owner, "accountId": a, "name": "Original", "status": "active"})
		put("v1:platform:package", "pkg-"+a, map[string]any{"ownerUserId": owner, "accountId": a, "name": a, "sourceKind": "repo", "repoUrl": "https://github.com/example/" + a, "status": "active", "autoDeploy": true, "deployedVersion": "upstream-sha", "autoDeployChangedAt": "2026-11-01T00:00:00Z"})
		put("v1:platform:site", "site-"+a, map[string]any{"ownerUserId": owner, "accountId": a, "hostname": a + ".example.test", "kind": "spa", "status": "live", "bundleRef": "blob://sites/site-" + a + "/v1/"})
	}
	group, membership := "g-"+account, "m-"+account
	seedCall(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Fylo", kind: "account", accountId: %s, status: "active")`, quote(group), quote(account)))
	seedCall(fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`, quote(membership), quote(group), quote(user), quote(account)))
	ids["v1:identity:group:"+group], ids["v1:identity:groupMembership:"+membership] = true, true
	for _, grant := range []struct{ kind, subject, verb, resource string }{
		{"group", group, "read", "app:deployables"}, {"group", group, "execute", "app:deployables/reviews"},
		{"user", user, "read", "app:email"}, {"user", user, "execute", "app:deployables/wholesale"}, {"user", user, "execute", "app:deployables/sales"},
	} {
		id := auth.GrantRowID(grant.kind, grant.subject, grant.verb, grant.resource)
		seedCall(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: %s, subjectId: %s, verb: %s, resourceType: %s, effect: "allow", grantedBy: "business-role-fixture")`, quote(id), quote(grant.kind), quote(grant.subject), quote(grant.verb), quote(grant.resource)))
		ids["v1:rbac:grant:"+id] = true
	}
	auth.SetCapabilityCatalog(fyloBusinessCatalog{organizationGroupsWireCatalog{role: role, account: account}})
	put("v1:identity:user", user, map[string]any{"role": role})
	actor := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.Role(role)})
	for _, verb := range []string{auth.VerbRead, auth.VerbCreate, auth.VerbUpdate} {
		require.False(t, eng.GlobalDataCapable(actor, verb))
		require.True(t, eng.OrganizationCapable(actor, account, verb, auth.ResourceData))
		require.False(t, eng.OrganizationCapable(actor, foreign, verb, auth.ResourceData))
	}
	require.Equal(t, 20, auth.RoleRank(auth.Role(role)))
	require.False(t, auth.IsClusterOperator(auth.Role(role)))
	for _, part := range []string{"reviews", "wholesale", "sales"} {
		require.True(t, eng.OrganizationCapable(actor, account, auth.VerbExecute, "app:deployables/"+part))
		require.False(t, eng.OrganizationCapable(actor, foreign, auth.VerbExecute, "app:deployables/"+part))
	}
	session, stream := gateTestSession(t, eng, auth.Role(role))
	session.access.UserId = user
	stream.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user})
	allowed := func(t *testing.T, id, query string) {
		t.Helper()
		driveGateQuery(t, session, id, query)
		waitForQueryResult(t, stream, id, 15*time.Second)
	}
	denied := func(t *testing.T, id, query string) {
		t.Helper()
		driveGateQuery(t, session, id, query)
		_, refusal := awaitAccountOutcome(t, stream, id, 15*time.Second)
		require.NotNil(t, refusal, "business role performed deployment control: %s", id)
		message := strings.ToLower(refusal.GetMessage())
		require.True(t, strings.Contains(message, "permission") || strings.Contains(message, "organization") || strings.Contains(message, "capability") || strings.Contains(message, "internal origin") || strings.Contains(message, "server-only") || strings.Contains(message, "row-authz"), "not an authorization refusal: %s", message)
	}
	latest := func(t *testing.T, kind, id string) map[string]any {
		t.Helper()
		var node memorynodes.MemoryNode
		require.NoError(t, db.NewSelect().Model(&node).Where("id = ?", kind+":"+id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()))
		var payload map[string]any
		require.NoError(t, json.Unmarshal(node.Payload, &payload))
		return payload
	}
	allowed(t, "business-create", raw("v1:campaigns:audience", "new-"+account, map[string]any{"name": "Business audience", "accountId": account}))
	allowed(t, "business-update", raw("v1:campaigns:audience", "aud-"+account, map[string]any{"name": "Business update"}))
	denied(t, "foreign-update", raw("v1:campaigns:audience", "aud-"+foreign, map[string]any{"name": "Forbidden"}))
	denied(t, "foreign-create", raw("v1:campaigns:audience", "new-"+foreign, map[string]any{"name": "Forbidden", "accountId": foreign}))
	for _, a := range []string{account, foreign} {
		id := "audience-read-" + a
		driveGateQuery(t, session, id, fmt.Sprintf(`query audienceById(audienceId: %s)`, quote("aud-"+a)))
		result := waitForQueryResult(t, stream, id, 15*time.Second)
		want := 1
		if a == foreign {
			want = 0
		}
		require.Equal(t, want, rowsInResult(result))
	}
	for _, tc := range []struct {
		name, concept, id string
		delta             map[string]any
	}{
		{"site-publish", "v1:platform:site", "site-" + account, map[string]any{"bundleRef": "blob://sites/changed/v2/"}},
		{"site-pause", "v1:platform:site", "site-" + account, map[string]any{"status": "disabled"}},
		{"site-retire", "v1:platform:site", "site-" + account, map[string]any{"status": "archived"}},
		{"site-runtime", "v1:platform:site", "site-" + account, map[string]any{"settings": map[string]any{"changed": true}}},
		{"package-policy", "v1:platform:package", "pkg-" + account, map[string]any{"autoDeploy": false}},
		{"package-source", "v1:platform:package", "pkg-" + account, map[string]any{"repoRef": "other"}},
		{"package-retire", "v1:platform:package", "pkg-" + account, map[string]any{"status": "archived"}},
		{"package-epoch-only", "v1:platform:package", "pkg-" + account, map[string]any{"autoDeployChangedAt": "2026-11-13T00:00:00Z"}},
		{"package-observed-version", "v1:platform:package", "pkg-" + account, map[string]any{"deployedVersion": "forged"}},
		{"package-upstream-version", "v1:platform:package", "pkg-" + account, map[string]any{"latestKnownVersion": "forged"}},
		{"package-update-cue", "v1:platform:package", "pkg-" + account, map[string]any{"updateAvailable": true}},
		{"package-declarations", "v1:platform:package", "pkg-" + account, map[string]any{"declares": []any{map[string]any{"name": "forged"}}}},
		{"package-manifest-name", "v1:platform:package", "pkg-" + account, map[string]any{"name": "forged"}},
		{"package-force-new-epoch", "v1:platform:package", "pkg-" + account, map[string]any{"deployedVersion": "pretend-not-current", "autoDeployChangedAt": "2026-11-13T00:00:00Z"}},
		{"package-forge-success", "v1:platform:packageDeployment", "fake-run-" + account, map[string]any{"packageId": "pkg-" + account, "accountId": account, "status": "succeeded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "package-force-new-epoch" {
				defer func() {
					persisted := latest(t, tc.concept, tc.id)
					observed := map[string]any{"deployedVersion": persisted["deployedVersion"], "autoDeployChangedAt": persisted["autoDeployChangedAt"]}
					require.Equal(t, map[string]any{"deployedVersion": "upstream-sha", "autoDeployChangedAt": "2026-11-01T00:00:00Z"}, observed, "persisted automatic-deployment control fields changed")
				}()
			}
			denied(t, tc.name, raw(tc.concept, tc.id, tc.delta))
		})
	}
	// Existing organization collaboration deliberately accepts an owner field
	// in an edit but restores the stored owning principal. Assert persistence,
	// not an error contract that this harmless normalization does not promise.
	allowed(t, "package-owner-normalized", raw("v1:platform:package", "pkg-"+account, map[string]any{"ownerUserId": "different-operator"}))
	require.Equal(t, peer, memqlengine.BareShortId(fmt.Sprint(latest(t, "v1:platform:package", "pkg-"+account)["ownerUserId"])))
	for _, tc := range []struct{ name, query string }{
		{"named-policy", fmt.Sprintf(`mutation setPackageAutoDeploy(packageId: %s, autoDeploy: true)`, quote("pkg-"+account))},
		{"named-source", fmt.Sprintf(`mutation updatePackageSource(packageId: %s, repoRef: "other")`, quote("pkg-"+account))},
		{"named-publish", fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "disabled")`, quote("site-"+account))},
		{"named-forge-version", fmt.Sprintf(`mutation recordPackageDeployedVersion(packageId: %s, deployedVersion: "forged", updateAvailable: true)`, quote("pkg-"+account))},
		{"direct-analyze", fmt.Sprintf(`builtin packageAnalyze(packageId: %s)`, quote("pkg-"+account))},
		{"direct-deploy", fmt.Sprintf(`builtin packageDeploy(packageId: %s, confirm: true)`, quote("pkg-"+account))},
		{"direct-retire", fmt.Sprintf(`builtin packageArchive(packageId: %s, confirmName: %s)`, quote("pkg-"+account), quote(account))},
	} {
		t.Run(tc.name, func(t *testing.T) { denied(t, tc.name, tc.query) })
	}
	// Raw infrastructure and mixed requests retain the global coarse gate.
	for _, q := range []string{
		raw("v1:platform:globalVariable", "MEMQL_EXAMPLE", map[string]any{"value": "changed"}),
		raw("v1:pipelines:pipeline", suffix+"-pipeline", map[string]any{"enabled": true}),
		raw("v1:work:run", suffix+"-work", map[string]any{"status": "running"}),
		`mutation createAgent(agentId: "business-forbidden", name: "Forbidden")`,
	} {
		ok, _ := session.allowDataPlaneAccess(actor, q)
		require.False(t, ok, "scoped data verbs admitted infrastructure: %s", q)
	}
	// The check must see the result of before-write hooks, not only the
	// client's delta. The harmless edit would otherwise retain all facts.
	hookSource := "authored:test-" + user
	eng.SetBeforeWriteHookSource(hookSource, map[string][]memqlengine.BeforeWriteHook{
		"v1:platform:package": {{Name: "package-observation-" + user, On: "update", Apply: func(ctx context.Context, row map[string]any) error {
			if access, _ := auth.AccessFromContext(ctx); access != nil && access.UserId == user {
				row["deployedVersion"] = "forged-after-hook"
			}
			return nil
		}}},
	})
	t.Cleanup(func() { eng.SetBeforeWriteHookSource(hookSource, nil) })
	denied(t, "hook-cannot-forge-version", raw("v1:platform:package", "pkg-"+account, map[string]any{"ownerUserId": peer}))
	require.Equal(t, "upstream-sha", latest(t, "v1:platform:package", "pkg-"+account)["deployedVersion"])
	eng.SetBeforeWriteHookSource(hookSource, nil)
	// Mutating a nested map or slice in place must not rewrite the stored
	// observation used by the guard. A shallow prior-map copy aliases both.
	for _, tc := range []struct {
		name  string
		apply func(map[string]any)
	}{
		{"declares-map", func(row map[string]any) {
			row["declares"].([]any)[0].(map[string]any)["name"] = "forged-in-place"
		}},
		{"dsl-domain-slice", func(row map[string]any) {
			row["dslDomains"].([]any)[0] = "forged-in-place"
		}},
	} {
		put("v1:platform:package", "pkg-"+account, map[string]any{
			"declares":   []any{map[string]any{"name": "Storefront", "kind": "spa"}},
			"dslDomains": []any{"fylo"},
		})
		t.Run("hook-in-place-"+tc.name, func(t *testing.T) {
			before := latest(t, "v1:platform:package", "pkg-"+account)
			eng.SetBeforeWriteHookSource(hookSource, map[string][]memqlengine.BeforeWriteHook{
				"v1:platform:package": {{Name: "package-nested-observation-" + user, On: "update", Apply: func(ctx context.Context, row map[string]any) error {
					if access, _ := auth.AccessFromContext(ctx); access != nil && access.UserId == user {
						tc.apply(row)
					}
					return nil
				}}},
			})
			defer eng.SetBeforeWriteHookSource(hookSource, nil)
			defer func() {
				after := latest(t, "v1:platform:package", "pkg-"+account)
				require.Equal(t, before["declares"], after["declares"], "persisted declaration changed through an in-place hook")
				require.Equal(t, before["dslDomains"], after["dslDomains"], "persisted DSL domain changed through an in-place hook")
			}()
			denied(t, "hook-in-place-"+tc.name, raw("v1:platform:package", "pkg-"+account, map[string]any{"ownerUserId": peer}))
		})
	}
	// Trusted recorders retain their existing write path. No feed, provider,
	// deployer or worker is started by this local engine fixture.
	seedCall(fmt.Sprintf(`mutation recordPackageDeployedVersion(packageId: %s, deployedVersion: "recorded-success", updateAvailable: false)`, quote("pkg-"+account)))
	seedCall(fmt.Sprintf(`mutation recordPackageUpstreamVersion(packageId: %s, latestKnownVersion: "recorded-upstream", updateAvailable: true)`, quote("pkg-"+account)))
	seedCall(fmt.Sprintf(`mutation recordPackageName(packageId: %s, name: "Recorded source name")`, quote("pkg-"+account)))
	stored := latest(t, "v1:platform:package", "pkg-"+account)
	require.Equal(t, "recorded-success", stored["deployedVersion"])
	require.Equal(t, "recorded-upstream", stored["latestKnownVersion"])
	require.Equal(t, "Recorded source name", stored["name"])
	// The actual source-policy grant admits the existing public mutation,
	// including reselecting Automatic and therefore changing only its epoch.
	for _, pair := range []auth.VerbResource{{Verb: "read", Resource: "app:pipelines"}, {Verb: "execute", Resource: "app:pipelines/repositories"}} {
		id := auth.GrantRowID("user", user, pair.Verb, pair.Resource)
		seedCall(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: %s, resourceType: %s, effect: "allow", grantedBy: "business-role-fixture")`, quote(id), quote(user), quote(pair.Verb), quote(pair.Resource)))
		ids["v1:rbac:grant:"+id] = true
	}
	allowed(t, "authorized-policy-reselection", fmt.Sprintf(`mutation setPackageAutoDeploy(packageId: %s, autoDeploy: true)`, quote("pkg-"+account)))
	stored = latest(t, "v1:platform:package", "pkg-"+account)
	require.Equal(t, true, stored["autoDeploy"])
	require.NotEmpty(t, stored["autoDeployChangedAt"])
	require.NotEqual(t, "2026-11-01T00:00:00Z", stored["autoDeployChangedAt"])
	require.Equal(t, peer, memqlengine.BareShortId(fmt.Sprint(stored["ownerUserId"])))
}
