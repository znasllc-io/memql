package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

type personalSourceCatalog struct {
	*capabilityFake
	account string
}

func (c *personalSourceCatalog) Scope(role string) string {
	if role == "source-manager" {
		return c.account
	}
	return ""
}

func TestPersonalSourceScopedAdmissionUsesRealDeclarationsAndOwnedRows(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	e.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	suffix := uniqueSuffix("personal-source")
	account, user, other := "account-"+suffix, "user-"+suffix, "other-"+suffix
	if err := organizationInsert(t, e, groupSeedCtx(), conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatal(err)
	}
	seedPrincipal(t, e, user, auth.Role("source-manager"))
	seedGroup(t, e, "g-"+account, "Source users", "account", account)
	seedMembership(t, e, "g-"+account, user, "active")
	// Another membership denies these app permissions. Its deny must not hide
	// the personal setup authorized through the role's own account.
	beta := "beta-" + suffix
	if err := organizationInsert(t, e, groupSeedCtx(), conceptAccountsAccount, beta, map[string]any{"name": beta, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, e, "g-"+beta, "Other source users", "account", beta)
	seedMembership(t, e, "g-"+beta, user, "active")
	writeGrant(t, e, auth.SubjectKindGroup, "g-"+beta, auth.VerbRead, "app:deployables", auth.GrantDeny)
	writeGrant(t, e, auth.SubjectKindGroup, "g-"+beta, auth.VerbExecute, "app:deployables/sources", auth.GrantDeny)
	for _, owner := range []string{user, other} {
		ctx := auth.ContextWithInternalOrigin(rankActorCtx(owner, auth.RoleOwner))
		q := fmt.Sprintf(`mutation recordSourceConnection(connectionId: %s, credentialId: %s, installationId: "42", providerAccountId: "100", accountLogin: "acme", accountType: "Organization")`, langparser.QuoteString("conn-"+owner), langparser.QuoteString("grant-"+owner))
		if _, err := e.Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &personalSourceCatalog{capabilityFake: &capabilityFake{
		ranks: map[string]int{"source-manager": 140, "owner": 400},
		grants: map[string]map[auth.VerbResource]bool{"source-manager": {
			{Verb: auth.VerbRead, Resource: auth.ResourceData}:            true,
			{Verb: auth.VerbRead, Resource: "app:deployables"}:            true,
			{Verb: auth.VerbExecute, Resource: "app:deployables/sources"}: true,
		}},
	}, account: account}
	auth.SetCapabilityCatalog(catalog)
	ctx := rankActorCtx(user, auth.Role("source-manager"))
	if e.GlobalDataCapable(ctx, auth.VerbRead) {
		t.Fatal("scoped role acquired targetless data authority")
	}
	reads := []string{`query sourceConnectionsMine()`, `query sourceConnectionById(connectionId: "missing")`, `query sourceCredentialsMine()`, `query sourceCredentialById(credentialId: "missing")`}
	for _, q := range reads {
		if !e.OrganizationDataPlaneEnforced(ctx, q) {
			t.Fatalf("personal read not admitted: %s", q)
		}
		result, err := e.Execute(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if q == reads[0] {
			rows := MaterializeRows(result)
			if len(rows) != 1 || BareShortId(stringFromAny(rows[0]["ownerUserId"])) != user {
				t.Fatalf("personal feed leaked or hid rows: %v", rows)
			}
		}
	}
	calls := []struct{ name, query string }{
		{"sourceConnectionCreate", `builtin sourceConnectionCreate(credentialId: "own", installationId: "42")`},
		{"sourceConnectionRemove", `builtin sourceConnectionRemove(connectionId: "own")`},
		{"sourceCredentialRevoke", `builtin sourceCredentialRevoke(credentialId: "own")`},
		{"githubConnectBegin", `builtin githubConnectBegin(returnTo: "https://os.example.test")`},
		{"sourceInstallations", `builtin sourceInstallations(credentialId: "own")`},
		{"sourceRepositories", `builtin sourceRepositories(connectionId: "own")`},
		{"sourceProbe", `builtin sourceProbe(connectionId: "own", repoUrl: "https://github.com/acme/repo")`},
	}
	for _, call := range calls {
		fn, ok := e.functions.Lookup(call.name)
		if !ok {
			t.Fatal(call.name)
		}
		if !personalSourceBuiltin(fn) {
			t.Fatalf("real builtin isn't audited: %+v", fn)
		}
		old, had := e.builtinExecutorHandlers[fn.Executor]
		executor := fn.Executor
		reached := false
		e.builtinExecutorHandlers[executor] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			reached = true
			return nil, nil
		}
		t.Cleanup(func() {
			if had {
				e.builtinExecutorHandlers[executor] = old
			} else {
				delete(e.builtinExecutorHandlers, executor)
			}
		})
		if !e.OrganizationDataPlaneEnforced(ctx, call.query) {
			t.Fatalf("personal builtin not admitted: %s", call.query)
		}
		if _, err := e.Execute(ctx, call.query); err != nil {
			t.Fatalf("%s: %v", call.query, err)
		}
		if !reached {
			t.Fatalf("personal handler not reached: %s", call.name)
		}
	}
	for _, q := range []string{`builtin sourceCredentialCreate(label: "x")`, `concept==v1:platform:sourceConnection`, `query sourceCredentialSealedById(credentialId: "own")`, `query githubAppGrantForCaller()`, `mutation recordSourceConnection(connectionId: "forged")`} {
		if e.OrganizationDataPlaneEnforced(ctx, q) {
			t.Fatalf("unverified door admitted: %s", q)
		}
	}
	// A direct denial overrides the scoped role and must affect both transport
	// admission and actual named execution on a subsequent request.
	writeGrant(t, e, auth.SubjectKindUser, user, auth.VerbExecute, "app:deployables/sources", auth.GrantDeny)
	if e.OrganizationDataPlaneEnforced(ctx, calls[0].query) {
		t.Fatal("revoked source management retained transport admission")
	}
	if _, err := e.Execute(ctx, calls[0].query); err == nil {
		t.Fatal("revoked source management retained engine admission")
	}
	if !e.OrganizationDataPlaneEnforced(ctx, reads[0]) {
		t.Fatal("execute denial incorrectly removed metadata read")
	}
	writeGrant(t, e, auth.SubjectKindUser, user, auth.VerbRead, auth.ResourceData, auth.GrantDeny)
	if e.OrganizationDataPlaneEnforced(ctx, reads[0]) {
		t.Fatal("read/data denial retained personal feed")
	}
	if _, err := e.Execute(ctx, reads[0]); err == nil {
		t.Fatal("engine personal read bypassed read/data denial")
	}
}

func TestPersonalSourceAdmissionRejectsLookalikeDeclarations(t *testing.T) {
	builtin := &Function{Name: "sourceConnectionCreate", FunctionKind: "builtin", Origin: "dsl/customer/builtins.memql", Executor: "integration.packages.sourceConnectionCreate"}
	if personalSourceBuiltin(builtin) {
		t.Fatal("customer builtin borrowed core exception")
	}
	builtin.Origin = "dsl/platform/builtins.memql"
	builtin.Executor = "integration.customer.sourceConnectionCreate"
	if personalSourceBuiltin(builtin) {
		t.Fatal("alternate executor borrowed core exception")
	}
	query := &Function{Name: "sourceConnectionsMine", FunctionKind: "query", Origin: "dsl/customer/queries.memql", BoundConcept: "v1:platform:sourceConnection"}
	if personalSourceQuery(query) {
		t.Fatal("customer query borrowed core exception")
	}
}
