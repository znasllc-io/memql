package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestOrganizationDataPlaneClassifierOnlyDefersCompleteGuardedRequests(t *testing.T) {
	engine, _, _ := sharedReadMergeEngine(t)
	ctx := rankActorCtx("classifier", auth.RoleReader)
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{`query templateById(templateId: "one")`, true},
		{`query clientAccountsAll()`, true},
		{`query effectiveCapabilitiesForActor()`, true},
		{`query groupCreate(name: "Team", accountId: "acme")`, true},
		{`query groupUpdate(groupId: "one", name: "Team")`, true},
		{`query groupArchive(groupId: "one")`, true},
		{`query groupMemberAdd(groupId: "one", userId: "two")`, true},
		{`query groupMemberRemove(groupId: "one", userId: "two")`, true},
		{`query groupPeople(groupId: "one")`, true},
		{`query groupEnsureForAccount(accountId: "acme")`, false},
		{`query packageDeploy(packageId: "one")`, true},
		{`query sourceCredentialCreate()`, false},
		{`mutation createTemplate(name: "New", subject: "Hi", textBody: "Hello")`, true},
		{`insert("v1:campaigns:template", id="one", payload={"name":"New","subject":"Hi"})`, true},

		{`concept==v1:campaigns:template`, false},
		{`concept==v1:campaigns:template || concept==v1:identity:user`, false},
		{`insert("v1:work:goal", id="one", payload={"name":"New"})`, false},
		{`insert("v1:identity:group", id="one", payload={"name":"New"})`, false},
		{`not a query`, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := engine.OrganizationDataPlaneEnforced(ctx, tc.query); got != tc.want {
				t.Fatalf("classification=%v want=%v", got, tc.want)
			}
		})
	}
	if organizationRootShape(&shapeRelationFunc{Relation: "parent", Template: &shapeNodeFunc{}}) {
		t.Fatal("related row projection classified as root-only")
	}
	if engine.organizationPlainFilter(&BuiltinFunctionExpression{Name: "danger"}, map[string]bool{}) {
		t.Fatal("builtin filter was treated as pure")
	}
	if engine.organizationPlainFilter(&LogicalExpression{Op: LogicalOr, Left: &constantBoolExpression{value: true}, Right: &AIExpression{}}, map[string]bool{}) {
		t.Fatal("mixed effect expression was treated as pure")
	}
}

func TestOrganizationGlobalDataFallbackDoesNotPromoteUnknownRoles(t *testing.T) {
	engine := &MemQLEngine{}
	for _, role := range []auth.Role{"", "not-real", auth.RoleReader} {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "fallback", Role: role})
		if !engine.GlobalDataCapable(ctx, auth.VerbRead) || engine.GlobalDataCapable(ctx, auth.VerbCreate) {
			t.Fatalf("fallback role %q has wrong capabilities", role)
		}
	}
}

func TestOrganizationReadDataDenialAppliesToAccountRowsAndOperators(t *testing.T) {
	engine, _, _ := sharedReadMergeEngine(t)
	oldGrant, oldMembership := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrant); auth.SetMembershipSource(oldMembership) })
	suffix := uniqueSuffix("data-read")
	account, user := "account-"+suffix, "user-"+suffix
	if err := organizationInsert(t, engine, groupSeedCtx(), conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatal(err)
	}
	seedPrincipal(t, engine, user, auth.RoleOwner)
	ctx := rankActorCtx(user, auth.RoleOwner)
	writeGrant(t, engine, auth.SubjectKindUser, user, auth.VerbRead, auth.ResourceData, auth.GrantDeny)
	ctx = contextWithAccountScopeMemo(ctx, engine)
	if engine.GlobalDataCapable(ctx, auth.VerbRead) {
		t.Fatal("operator bypassed direct read denial")
	}
	expr := engine.accountScopeComparison(ctx, &AccountScopeExpression{Field: "id", Organization: true, RowID: true})
	if b, ok := expr.(*constantBoolExpression); !ok || b.value {
		t.Fatal("operator SQL read bypassed data denial")
	}
	if got := rowAuthzAdmits(ctx, conceptAccountsAccount, account, rowPayload(t, map[string]any{"name": account})); got != rowAuthzDeny {
		t.Fatal("operator row gate bypassed data denial")
	}
}
