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
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/groups"
)

// The browser uses ExecuteQueryMsg, so engine-only tests cannot prove that a
// grant in A still works when B denies the same capability at the front door.
func TestOrganizationDataPermissionsThroughExecuteQuery(t *testing.T) {
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrants); auth.SetMembershipSource(oldMembers) })

	suffix := fmt.Sprintf("org-wire-%d", time.Now().UnixNano())
	user, a, b := suffix+"-user", suffix+"-a", suffix+"-b"
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("org-wire-fixture")))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: "org-wire-fixture"})
	quote := langparser.QuoteString
	execute := func(q string) {
		t.Helper()
		_, err := eng.Execute(seed, q)
		require.NoError(t, err, q)
	}
	put := func(kind, id string, payload map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		execute(fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, quote(kind), quote(id), encoded))
	}
	put("v1:identity:user", user, map[string]any{"displayName": "Scoped reader", "primaryEmail": user + "@example.test", "role": "reader", "active": true})
	for _, account := range []string{a, b} {
		put("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
		execute(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`, quote("g-"+account), quote(account), quote(account)))
		execute(fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`, quote("m-"+account), quote("g-"+account), quote(user), quote(account)))
		put("v1:campaigns:audience", "aud-"+account, map[string]any{"name": account, "status": "active", "accountId": account, "ownerUserId": user})
		for _, vr := range []struct{ verb, resource string }{{"read", "data"}, {"create", "data"}, {"update", "data"}, {"read", "app:campaigns"}} {
			effect := "deny"
			if account == a && vr.verb != "create" {
				effect = "allow"
			}
			id := auth.GrantRowID("group", "g-"+account, vr.verb, vr.resource)
			execute(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "group", subjectId: %s, verb: %s, resourceType: %s, effect: %s, grantedBy: "org-wire-fixture")`, quote(id), quote("g-"+account), quote(vr.verb), quote(vr.resource), quote(effect)))
		}
	}
	session, stream := gateTestSession(t, eng, auth.RoleReader)
	session.access.UserId = user
	stream.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user})
	write := func(account, id, name string) string {
		payload, _ := json.Marshal(map[string]any{"name": name, "accountId": account})
		return fmt.Sprintf(`insert("v1:campaigns:audience", id=%s, payload=%s)`, quote(id), payload)
	}
	driveGateQuery(t, session, "update-a", write(a, "aud-"+a, "Updated through wire"))
	waitForQueryResult(t, stream, "update-a", 15*time.Second)
	driveGateQuery(t, session, "archive-a", fmt.Sprintf(`mutation archiveAudience(audienceId: %s)`, quote("aud-"+a)))
	waitForQueryResult(t, stream, "archive-a", 15*time.Second)
	for _, tc := range []struct{ id, query string }{
		{"update-b", write(b, "aud-"+b, "Forbidden")},
		{"archive-b", fmt.Sprintf(`mutation archiveAudience(audienceId: %s)`, quote("aud-"+b))},
		{"create-a", write(a, "new-"+a, "Forbidden create")},
		{"unscoped-write", `mutation createAgent(agentId: "org-wire-forbidden", name: "Forbidden")`},
	} {
		driveGateQuery(t, session, tc.id, tc.query)
		_, queryErr := awaitAccountOutcome(t, stream, tc.id, 15*time.Second)
		require.NotNil(t, queryErr, tc.id)
		message := strings.ToLower(queryErr.GetMessage())
		require.True(t, strings.Contains(message, "permission") || strings.Contains(message, "organization") || strings.Contains(message, "capability") || strings.Contains(message, "row-authz"), "%s: %s", tc.id, message)
	}
	for _, account := range []string{a, b} {
		id := "read-" + account
		driveGateQuery(t, session, id, fmt.Sprintf(`query audienceById(audienceId: %s)`, quote("aud-"+account)))
		result := waitForQueryResult(t, stream, id, 15*time.Second)
		want := 0
		if account == a {
			want = 1
		}
		require.Equal(t, want, rowsInResult(result), account)
	}
	// An organization grant must not become global authority merely because
	// no other organization's deny happens to mask it in a flattened set.
	for _, account := range []string{a, b} {
		effect := "allow"
		id := auth.GrantRowID("group", "g-"+account, "create", "data")
		execute(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "group", subjectId: %s, verb: "create", resourceType: "data", effect: %s, grantedBy: "org-wire-fixture")`, quote(id), quote("g-"+account), quote(effect)))
	}
	driveGateQuery(t, session, "create-a-allowed", write(a, "allowed-new-"+a, "Allowed organization create"))
	waitForQueryResult(t, stream, "create-a-allowed", 15*time.Second)
	driveGateQuery(t, session, "unscoped-still-denied", `mutation createAgent(agentId: "org-wire-forbidden", name: "Forbidden")`)
	_, denied := awaitAccountOutcome(t, stream, "unscoped-still-denied", 15*time.Second)
	require.NotNil(t, denied, "organization create grants became global data authority")
	require.Contains(t, denied.GetMessage(), "permission denied")
	var stored concept.MemoryNode
	require.NoError(t, db.NewSelect().Model(&stored).Where("concept = ?", "v1:campaigns:audience").Where("id = ?", "v1:campaigns:audience:aud-"+a).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()))
	var payload map[string]any
	require.NoError(t, json.Unmarshal(stored.Payload, &payload))
	require.Equal(t, "Updated through wire", payload["name"])
	require.Equal(t, user, memqlengine.BareShortId(stored.CreatedBy))
}

// Only the role catalog is fixed for this test. Memberships, grants, resource
// reads, integration guards and writes all resolve through the real engine.
type organizationGroupsWireCatalog struct{ role, account string }

func (c organizationGroupsWireCatalog) Rank(slug string) (int, bool) {
	if slug == c.role {
		return 250, true
	}
	ranks := map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50, "reader": 50}
	rank, ok := ranks[slug]
	return rank, ok
}
func (c organizationGroupsWireCatalog) Name(slug string) string { return slug }
func (c organizationGroupsWireCatalog) Scope(slug string) string {
	if slug == c.role {
		return c.account
	}
	return ""
}
func (c organizationGroupsWireCatalog) Active(slug string) bool { _, ok := c.Rank(slug); return ok }
func (c organizationGroupsWireCatalog) Holds(slug, verb, resource string) bool {
	if slug == "owner" {
		return true
	}
	return slug == c.role && verb == auth.VerbRead && (resource == auth.ResourceData || resource == "app:users")
}
func (c organizationGroupsWireCatalog) Grants(slug string) []auth.VerbResource {
	if slug == c.role {
		return []auth.VerbResource{{Verb: auth.VerbRead, Resource: auth.ResourceData}, {Verb: auth.VerbRead, Resource: "app:users"}}
	}
	return nil
}

type organizationGroupsWireEngine struct{ *memqlengine.MemQLEngine }

func (e organizationGroupsWireEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

func TestOrganizationGroupManagementThroughExecuteQuery(t *testing.T) {
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	eng.InstallGrantResolution()
	require.NoError(t, eng.RegisterIntegration(groups.New(organizationGroupsWireEngine{eng}, func(string, ...any) {})))
	suffix := fmt.Sprintf("org-groups-wire-%d", time.Now().UnixNano())
	user, peerA, peerB, a, b, role := suffix+"-user", suffix+"-peer-a", suffix+"-peer-b", suffix+"-a", suffix+"-b", suffix+"-role"
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("org-groups-wire-fixture")))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: "org-groups-wire-fixture"})
	quote := langparser.QuoteString
	execute := func(q string) { t.Helper(); _, err := eng.Execute(seed, q); require.NoError(t, err, q) }
	put := func(kind, id string, payload map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		execute(fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, quote(kind), quote(id), encoded))
	}
	for _, id := range []string{user, peerA, peerB} {
		put("v1:identity:user", id, map[string]any{"displayName": id, "primaryEmail": id + "@example.test", "role": "reader", "active": true})
	}
	for _, account := range []string{a, b} {
		put("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
		execute(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`, quote("g-"+account), quote(account), quote(account)))
		peer := peerA
		if account == b {
			peer = peerB
		}
		for _, member := range []string{user, peer} {
			execute(fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`, quote("m-"+account+"-"+member), quote("g-"+account), quote(member), quote(account)))
		}
		effect := auth.GrantAllow
		if account == b {
			effect = auth.GrantDeny
		}
		grantID := auth.GrantRowID(auth.SubjectKindGroup, "g-"+account, auth.VerbUpdate, auth.ResourceGroup)
		execute(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "group", subjectId: %s, verb: "update", resourceType: "group", effect: %s, grantedBy: "org-groups-wire-fixture")`, quote(grantID), quote("g-"+account), quote(effect)))
	}
	auth.SetCapabilityCatalog(organizationGroupsWireCatalog{role: role, account: a})
	put("v1:identity:user", user, map[string]any{"role": role})
	require.Equal(t, a, auth.RoleAccountScope(auth.Role(role)))
	require.False(t, auth.IsClusterOperator(auth.Role(role)), "an account-scoped high rank must not become a cluster operator")
	session, stream := gateTestSession(t, eng, auth.Role(role))
	session.access.UserId = user
	stream.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user})
	driveGateQuery(t, session, "people-a", fmt.Sprintf(`builtin groupPeople(groupId: %s)`, quote("g-"+a)))
	people := waitForQueryResult(t, stream, "people-a", 15*time.Second)
	// Builtin results are one wire object keyed by the returned node IDs.
	require.Len(t, people.GetResult().GetData(), 1)
	roster := people.GetResult().GetData()[0].GetStructValue().GetFields()
	require.Len(t, roster, 2, "only A's existing people belong in its management roster")
	require.Contains(t, roster, user)
	require.Contains(t, roster, peerA)
	require.NotContains(t, roster, peerB)
	driveGateQuery(t, session, "update-group-a", fmt.Sprintf(`builtin groupUpdate(groupId: %s, name: "Updated through organization management")`, quote("g-"+a)))
	waitForQueryResult(t, stream, "update-group-a", 15*time.Second)
	for _, name := range []string{"groupPeople", "groupUpdate"} {
		requestID := "forbidden-" + name
		query := fmt.Sprintf(`builtin %s(groupId: %s)`, name, quote("g-"+b))
		if name == "groupUpdate" {
			query = fmt.Sprintf(`builtin groupUpdate(groupId: %s, name: "Forbidden rename")`, quote("g-"+b))
		}
		driveGateQuery(t, session, requestID, query)
		_, queryErr := awaitAccountOutcome(t, stream, requestID, 15*time.Second)
		require.NotNil(t, queryErr, name+" must not reach B")
		require.Contains(t, strings.ToLower(queryErr.GetMessage()), "organization")
	}
	for _, account := range []string{a, b} {
		var row concept.MemoryNode
		require.NoError(t, db.NewSelect().Model(&row).Where("concept = ?", "v1:identity:group").Where("id = ?", "v1:identity:group:g-"+account).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()))
		var payload map[string]any
		require.NoError(t, json.Unmarshal(row.Payload, &payload))
		want := account
		if account == a {
			want = "Updated through organization management"
		}
		require.Equal(t, want, payload["name"], "actual group integration must persist A and leave B unchanged")
	}
}
