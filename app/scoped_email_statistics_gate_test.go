package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/emailstats"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

type scopedEmailStatisticsCatalog struct{ scopedOrganizationWireCatalog }

func (c scopedEmailStatisticsCatalog) Holds(slug, verb, resource string) bool {
	return slug == "owner" || slug == c.role && verb == auth.VerbRead && (resource == auth.ResourceData || resource == "app:email")
}
func (c scopedEmailStatisticsCatalog) Grants(slug string) []auth.VerbResource {
	if slug == c.role {
		return []auth.VerbResource{{Verb: auth.VerbRead, Resource: auth.ResourceData}, {Verb: auth.VerbRead, Resource: "app:email"}}
	}
	return nil
}

// Real public TCP wire, verified fixture identity, real membership/grant resolution and
// the real statistics handler. Statistics facts/watermarks live in connection-
// private temporary tables; no shared rollup position is changed by this test.
func TestScopedEmailStatisticsThroughWireHonorsOrganizationDenials(t *testing.T) {
	eng, db := workTemplateDBEngineAndDB(t)
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	eng.InstallGrantResolution()
	statsDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	statsDB.SetMaxOpenConns(1)
	statsDB.SetMaxIdleConns(1)
	statsDB.SetConnMaxLifetime(0)
	statsDB.SetConnMaxIdleTime(0)
	t.Cleanup(func() { require.NoError(t, statsDB.Close()) })
	for _, table := range []string{"email_stat_fact", "email_stat_watermark", "email_stat_walk_start"} {
		_, err := statsDB.ExecContext(context.Background(), `CREATE TEMPORARY TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`)
		require.NoError(t, err)
		var persistence string
		err = statsDB.NewRaw(`SELECT relpersistence::text FROM pg_class WHERE oid = ?::regclass`, table).Scan(context.Background(), &persistence)
		require.NoError(t, err)
		require.Equal(t, "t", persistence, "statistics fixture must shadow the shared table")
	}
	require.NoError(t, eng.RegisterIntegration(emailstats.NewIntegration(eng, func() *bun.DB { return statsDB }, eng.Logger)))
	suffix := fmt.Sprintf("scoped-email-statistics-%d", time.Now().UnixNano())
	user, peer, a, b, orphan, role := suffix+"-user", suffix+"-peer", suffix+"-a", suffix+"-foreign", suffix+"-orphan", suffix+"-role"
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("scoped-email-statistics-fixture")))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: "scoped-email-statistics-fixture"})
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
	})
	for _, person := range []string{user, peer} {
		put("v1:identity:user", person, map[string]any{"displayName": person, "primaryEmail": person + "@example.test", "role": "reader", "active": true})
	}
	for _, account := range []string{a, b} {
		put("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
		execute(fmt.Sprintf(`mutation writeGroup(groupId:%s,name:%s,kind:"account",accountId:%s,status:"active")`, quote("g-"+account), quote(account), quote(account)))
		execute(fmt.Sprintf(`mutation writeGroupMembership(membershipId:%s,groupId:%s,userId:%s,origin:"added",status:"active",accountId:%s)`, quote("m-"+account), quote("g-"+account), quote(user), quote(account)))
		ids = append(ids, "v1:identity:group:g-"+account, "v1:identity:groupMembership:m-"+account)
		owner := peer
		if account == b {
			owner = user
		}
		put("v1:campaigns:audience", "aud-"+account, map[string]any{"name": account, "status": "active", "ownerUserId": owner, "accountId": account})
		put("v1:campaigns:template", "tpl-"+account, map[string]any{"name": account, "subject": "Fixture", "textBody": "Fixture", "ownerUserId": owner, "accountId": account})
		put("v1:campaigns:campaign", "camp-"+account, map[string]any{"name": account, "status": "cancelled", "ownerUserId": owner, "accountId": account, "audienceId": "aud-" + account, "templateId": "tpl-" + account})
	}
	for _, source := range []string{"delivery", "engagementEvent", "consentEvent"} {
		_, err := statsDB.ExecContext(context.Background(), `INSERT INTO email_stat_watermark(source,walked_through) VALUES (?,now())`, source)
		require.NoError(t, err)
	}
	// The orphan account intentionally has no registry row, and the blank
	// account represents older unattributed facts. Neither may disappear from
	// an otherwise authorized operator's historical aggregate.
	for account, count := range map[string]int{a: 3, b: 17, orphan: 5, "": 7} {
		for n := 0; n < count; n++ {
			_, err := statsDB.ExecContext(context.Background(), `INSERT INTO email_stat_fact(kind,source_id,account_id,campaign_id,day) VALUES ('sent',?,?,?,(now() AT TIME ZONE 'UTC')::date-1)`, fmt.Sprintf("%s-%d", account, n), account, "camp-"+account)
			require.NoError(t, err)
		}
	}
	_, err := statsDB.ExecContext(context.Background(), `INSERT INTO email_stat_fact(kind,source_id,account_id,day) VALUES ('sent','anchor','historical-anchor',(now() AT TIME ZONE 'UTC')::date-45)`)
	require.NoError(t, err)
	auth.SetCapabilityCatalog(scopedEmailStatisticsCatalog{scopedOrganizationWireCatalog{role: role, account: a}})
	put("v1:identity:user", user, map[string]any{"role": role})
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.Role(role)})
	require.False(t, eng.GlobalDataCapable(ctx, auth.VerbRead))
	require.True(t, eng.OrganizationCapable(ctx, a, auth.VerbRead, auth.ResourceData))
	require.False(t, eng.OrganizationCapable(ctx, b, auth.VerbRead, auth.ResourceData))
	wire := newScopedReadWire(t, eng, user, auth.Role(role))
	call := func(id, extra string, want any, forbidForeign bool) map[string]any {
		t.Helper()
		p := scopedDeployableResult(t, wire.Query(t, id, `builtin emailStatistics(period:"7d"`+extra+`)`))
		require.Equal(t, want, p["totals"].(map[string]any)["sent"], id)
		if want == nil {
			require.Nil(t, p["measuredSince"])
			require.Empty(t, p["campaigns"])
			require.Empty(t, p["accounts"])
		}
		if forbidForeign {
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			require.NotContains(t, string(raw), b, id)
		}
		return p
	}
	for _, tc := range []struct {
		name, extra string
		want        any
	}{
		{"all", "", float64(3)}, {"own", `,accountIds:[` + quote(a) + `]`, float64(3)},
		{"both", `,accountIds:[` + quote(a) + `,` + quote(b) + `]`, float64(3)},
		{"foreign", `,accountIds:[` + quote(b) + `]`, nil}, {"orphan", `,accountIds:[` + quote(orphan) + `]`, nil},
		{"blank", `,accountIds:[""]`, nil}, {"unknown", `,accountIds:["not-an-account"]`, nil},
	} {
		call(tc.name, tc.extra, tc.want, true)
	}
	grant := func(kind, subject, resource, effect string) {
		t.Helper()
		id := auth.GrantRowID(kind, subject, auth.VerbRead, resource)
		ids = append(ids, "v1:rbac:grant:"+id)
		execute(fmt.Sprintf(`mutation writeGrant(grantId:%s,subjectKind:%s,subjectId:%s,verb:"read",resourceType:%s,effect:%s,grantedBy:"statistics-fixture")`, quote(id), quote(kind), quote(subject), quote(resource), quote(effect)))
	}
	for n, resource := range []string{auth.ResourceData, "app:email"} {
		grant(auth.SubjectKindUser, user, resource, auth.GrantDeny)
		call(fmt.Sprintf("revoked-%d", n), "", nil, true)
		grant(auth.SubjectKindUser, user, resource, auth.GrantAllow)
		call(fmt.Sprintf("restored-%d", n), "", float64(3), true)
		// Remove the direct override before testing group decisions below.
		id := auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbRead, resource)
		execute(fmt.Sprintf(`mutation deactivateGrant(grantId:%s)`, quote(id)))
	}
	grant(auth.SubjectKindGroup, "g-"+b, "app:email", auth.GrantDeny)
	call("foreign-deny-does-not-stop-own", "", float64(3), true)
	// Same user's full memberships and foreign ownership cannot lift a role
	// fence. Changing to an actual operator below is explicit test setup.
	put("v1:identity:user", user, map[string]any{"role": "owner"})
	wire.Close()
	wire = newScopedReadWire(t, eng, user, auth.RoleOwner)
	call("operator-with-account-deny", "", float64(15), true)
	call("operator-orphan", `,accountIds:[`+quote(orphan)+`]`, float64(5), true)
	call("operator-denied-account", `,accountIds:[`+quote(b)+`]`, nil, true)
	grant(auth.SubjectKindGroup, "g-"+b, "app:email", auth.GrantAllow)
	grant(auth.SubjectKindGroup, "g-"+b, auth.ResourceData, auth.GrantDeny)
	call("operator-account-data-deny", "", float64(15), true)
	for n, resource := range []string{auth.ResourceData, "app:email"} {
		grant(auth.SubjectKindUser, user, resource, auth.GrantDeny)
		call(fmt.Sprintf("operator-global-denied-%d", n), "", nil, true)
		id := auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbRead, resource)
		execute(fmt.Sprintf(`mutation deactivateGrant(grantId:%s)`, quote(id)))
	}
	put("v1:identity:user", user, map[string]any{"role": role})
	wire.Close()
	wire = newScopedReadWire(t, eng, user, auth.Role(role))
	call("scoped-before-membership-revocation", "", float64(3), true)
	execute(fmt.Sprintf(`mutation writeGroupMembership(membershipId:%s,groupId:%s,userId:%s,origin:"added",status:"removed",accountId:%s)`, quote("m-"+a), quote("g-"+a), quote(user), quote(a)))
	call("membership-revoked", "", nil, true)
	for n, q := range []string{
		`builtin emailStatisticsRollup()`, `builtin emailStatistics(period:"7d") || concept==v1:identity:user`,
		`builtin emailStatistics(period:builtin modelCatalog())`, `query agentById(agentId:"private")`,
		`mutation createAgent(agentId:"private",name:"Private")`, `builtin notEmailStatistics()`,
	} {
		wire.Refused(t, fmt.Sprintf("unsafe-source-%d", n), q)
	}
	require.False(t, eng.GlobalDataCapable(ctx, auth.VerbRead))
}
