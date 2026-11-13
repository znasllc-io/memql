package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/sitetraffic"
)

// The role catalog and transport authentication are fixtures. Both real
// integrations, nested reads, memberships and denials use the database. The
// public TCP stream resolves the seeded user from verified fixture claims,
// never an owner bypass or a first node's scope memo.
type scopedDeployableWireCatalog struct{ scopedOrganizationWireCatalog }

func (c scopedDeployableWireCatalog) Holds(slug, verb, resource string) bool {
	return slug == "owner" || slug == c.role && verb == auth.VerbRead && (resource == auth.ResourceData || resource == "app:deployables")
}
func (c scopedDeployableWireCatalog) Grants(slug string) []auth.VerbResource {
	if slug == c.role {
		return []auth.VerbResource{{Verb: auth.VerbRead, Resource: auth.ResourceData}, {Verb: auth.VerbRead, Resource: "app:deployables"}}
	}
	return nil
}

func TestScopedDeployableReadsThroughWireKeepOrganizationRowsAndTotals(t *testing.T) {
	eng, db := workTemplateDBEngineAndDB(t)
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	eng.InstallGrantResolution()
	require.NoError(t, eng.RegisterIntegration(packages.NewIntegration(eng, eng.Logger)))
	require.NoError(t, eng.RegisterIntegration(sitetraffic.NewIntegration(eng, func() *bun.DB { return db }, nil, eng.Logger)))

	suffix := fmt.Sprintf("scoped-deployables-%d", time.Now().UnixNano())
	user, peer, a, b, role := suffix+"-user", suffix+"-peer", suffix+"-a", suffix+"-foreign", suffix+"-role"
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("scoped-deployables-fixture")))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: "scoped-deployables-fixture"})
	quote := langparser.QuoteString
	execute := func(q string) { t.Helper(); _, err := eng.Execute(seed, q); require.NoError(t, err, q) }
	ids := []string{}
	put := func(kind, id string, payload map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		execute(fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, quote(kind), quote(id), encoded))
		ids = append(ids, kind+":"+id)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
		for _, table := range []string{"edge_stats_daily", "edge_stats_daily_top"} {
			_, err := db.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE site_id IN (?, ?)", "site-"+a, "site-"+b)
			require.NoError(t, err)
		}
	})
	for _, person := range []string{user, peer} {
		put("v1:identity:user", person, map[string]any{"displayName": person, "primaryEmail": person + "@example.test", "role": "reader", "active": true})
	}
	for _, account := range []string{a, b} {
		put("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
		execute(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`, quote("g-"+account), quote(account), quote(account)))
		execute(fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`, quote("m-"+account), quote("g-"+account), quote(user), quote(account)))
		ids = append(ids, "v1:identity:group:g-"+account, "v1:identity:groupMembership:m-"+account)
		// The caller even owns the foreign rows and belongs to both groups;
		// neither fact may override the role's organization scope.
		owner := peer
		if account == b {
			owner = user
		}
		apps := []any{}
		for n := 0; n < 12; n++ {
			apps = append(apps, map[string]any{"name": fmt.Sprintf("app-%02d", n), "displayName": fmt.Sprintf("%s app %02d", account, n), "kind": "spa"})
		}
		put("v1:platform:package", "package-"+account, map[string]any{"ownerUserId": owner, "accountId": account, "name": account, "sourceKind": "repo", "repoUrl": "https://github.com/example/" + account, "status": "active", "declares": apps})
		put("v1:platform:site", "site-"+account, map[string]any{"ownerUserId": owner, "accountId": account, "packageId": "package-" + account, "packageDeployableName": "app-00", "hostname": account + ".example.test", "kind": "spa", "status": "live", "bundleRef": ""})
		put("v1:platform:packageDeployment", "run-"+account, map[string]any{"ownerUserId": owner, "accountId": account, "packageId": "package-" + account, "requestedBy": owner, "status": "awaiting_confirm", "scopedTo": []any{"app-01"}, "report": map[string]any{"deployables": []any{apps[1]}}})
		visitors := 3
		if account == b {
			visitors = 500
		}
		_, err := db.ExecContext(context.Background(), `INSERT INTO edge_stats_daily(site_id,day,visitors,visits,bounces,page_views,requests,errors,not_found,bytes,bot_requests,duration_hist) VALUES (?,(now() AT TIME ZONE 'UTC')::date - 1,?,?,0,?,?,7,0,1000,0,'{}')`, "site-"+account, visitors, visitors, visitors, visitors)
		require.NoError(t, err)
		_, err = db.ExecContext(context.Background(), `INSERT INTO edge_stats_daily_top(site_id,day,dimension,value,visitors,visits,page_views) VALUES (?,(now() AT TIME ZONE 'UTC')::date - 1,'page',?,?,?,?)`, "site-"+account, "/"+account, visitors, visitors, visitors)
		require.NoError(t, err)
	}
	auth.SetCapabilityCatalog(scopedDeployableWireCatalog{scopedOrganizationWireCatalog{role: role, account: a}})
	put("v1:identity:user", user, map[string]any{"role": role})
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.Role(role)})
	require.False(t, eng.GlobalDataCapable(ctx, auth.VerbRead))
	require.True(t, eng.OrganizationCapable(ctx, a, auth.VerbRead, auth.ResourceData))
	require.False(t, eng.OrganizationCapable(ctx, b, auth.VerbRead, auth.ResourceData))
	stream := newScopedReadWire(t, eng, user, auth.Role(role))
	call := func(t *testing.T, id, query string) map[string]any {
		t.Helper()
		result := stream.Query(t, id, query)
		payload := scopedDeployableResult(t, result)
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		require.NotContains(t, string(raw), b, "foreign organization data reached %s", id)
		return payload
	}
	directory := func(id, extra string, want int) {
		t.Helper()
		p := call(t, id, `builtin deployableDirectoryPage(resource: "deployables"`+extra+`)`)
		require.Equal(t, float64(want), p["total"])
		rows := p["entries"].([]any)
		if want == 0 {
			require.Empty(t, rows)
			return
		}
		require.Len(t, rows, 10)
		cursor, ok := p["nextCursor"].(string)
		require.True(t, ok)
		require.NotEmpty(t, cursor)
		next := call(t, id+"-next", `builtin deployableDirectoryPage(resource: "deployables"`+extra+`, cursor: `+quote(cursor)+`)`)
		require.Equal(t, float64(want), next["total"])
		require.Len(t, next["entries"], 2)
		require.Empty(t, next["nextCursor"])
		seen := map[string]bool{}
		for _, row := range append(rows, next["entries"].([]any)...) {
			entry := row.(map[string]any)
			key := entry["id"].(string)
			require.False(t, seen[key])
			seen[key] = true
		}
		require.Len(t, seen, 12)
	}
	directory("all", "", 12)
	directory("own", `, accountId: `+quote(a), 12)
	directory("foreign", `, accountId: `+quote(b), 0)
	directory("foreign-search", `, search: `+quote(b), 0)
	for _, tc := range []struct {
		name, extra string
		want        bool
	}{
		{"all", "", true}, {"own-account", `, accountIds: [` + quote(a) + `]`, true},
		{"both-accounts", `, accountIds: [` + quote(a) + `,` + quote(b) + `]`, true},
		{"foreign-account", `, accountIds: [` + quote(b) + `]`, false},
		{"both-sites", `, siteIds: [` + quote("site-"+a) + `,` + quote("site-"+b) + `]`, true},
		{"foreign-site", `, siteIds: [` + quote("site-"+b) + `]`, false},
		{"foreign-canonical-site", `, siteIds: [` + quote("v1:platform:site:site-"+b) + `]`, false},
		{"blank-account", `, accountIds: [""]`, false},
	} {
		t.Run("statistics-"+tc.name, func(t *testing.T) {
			p := call(t, "statistics-"+tc.name, `builtin siteStatistics(period: "7d"`+tc.extra+`)`)
			totals := p["totals"].(map[string]any)
			require.Nil(t, totals["requests"], "health requires its separate capability")
			if tc.want {
				require.Equal(t, float64(3), totals["visitors"])
				require.Len(t, p["sites"], 1)
			} else {
				require.Nil(t, totals["visitors"])
				require.Empty(t, p["sites"])
			}
		})
	}
	// Revocation must remain effective after collection admission; the same
	// verified session cannot retain a prior nested read's authorization.
	for n, resource := range []string{"app:deployables", auth.ResourceData} {
		grant := auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbRead, resource)
		ids = append(ids, "v1:rbac:grant:"+grant)
		execute(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "read", resourceType: %s, effect: "deny", grantedBy: "scoped-deployables-fixture")`, quote(grant), quote(user), quote(resource)))
		directory(fmt.Sprintf("denied-%d", n), "", 0)
		p := call(t, fmt.Sprintf("statistics-denied-%d", n), `builtin siteStatistics(period: "7d")`)
		require.Empty(t, p["sites"])
		require.Nil(t, p["totals"].(map[string]any)["visitors"])
		execute(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "read", resourceType: %s, effect: "allow", grantedBy: "scoped-deployables-fixture")`, quote(grant), quote(user), quote(resource)))
	}
	require.False(t, eng.GlobalDataCapable(ctx, auth.VerbRead), "collection admission promoted an organization role")
	for n, query := range []string{`builtin deployableDirectoryPage(resource: "sources")`, `builtin deployableDirectoryPage(resource: "unknown")`, `query agentById(agentId: "private")`, `mutation createAgent(agentId: "private", name: "Private")`, `builtin siteStatistics(period: "7d") || concept==v1:identity:user`, `builtin siteStatistics(period: builtin modelCatalog())`} {
		stream.Refused(t, fmt.Sprintf("denied-query-%d", n), query)
	}
}

func scopedDeployableResult(t *testing.T, result *memqlv1.QueryResultChunk) map[string]any {
	t.Helper()
	require.NotNil(t, result.GetResult())
	if rows := result.GetResult().GetData(); len(rows) > 0 {
		require.Len(t, rows, 1)
		nodes := rows[0].GetStructValue().AsMap()
		require.Len(t, nodes, 1, "builtin returns an object keyed by synthetic node id")
		for _, raw := range nodes {
			node, ok := raw.(map[string]any)
			require.True(t, ok)
			payload, ok := node["payload"].(map[string]any)
			require.True(t, ok, "builtin must return a synthetic node payload: %v", node)
			return payload
		}
	}
	nodes := result.GetResult().GetBundle().GetNodes()
	require.Len(t, nodes, 1)
	return nodes[0].GetPayload().AsMap()
}
