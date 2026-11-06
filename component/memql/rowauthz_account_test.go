package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The account grant of the owned tier (epic memql#5165, task memql#5169).
//
// These run with NO DATABASE: the resolution is injected on the context the
// way the rank tests inject theirs, so what is measured here is the GATE --
// which rows a given scope admits -- rather than the membership read. The
// db-gated file beside this one measures the read.

const (
	declaredAccountConcept     = "v1:rowauthzfixture:account"
	declaredAccountListConcept = "v1:rowauthzfixture:accountlist"
)

// accountFixture registers a concept declaring the account grant on `field`.
func accountFixture(t *testing.T, conceptName, field string) *langparser.RowAuthzDecl {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	decl := &langparser.RowAuthzDecl{
		Tier:    langparser.RowAuthzOwned,
		Owner:   "ownerUserId",
		Account: field,
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		conceptName: {Name: conceptName, NodeType: "fixture", RowAuthz: decl},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROL. An undeclared concept admits everything, so without
	// this every assertion below could pass while measuring nothing.
	got := rowAuthzDeclFor(conceptName)
	if got == nil {
		t.Fatal("the account fixture is not in the registry, so every assertion below would " +
			"measure an UNDECLARED concept -- which admits everything and passes for the wrong reason")
	}
	if got.Account != field {
		t.Fatalf("the fixture resolved to %+v, which carries no account grant on %q", *got, field)
	}
	return got
}

// accountCtx builds a caller with a pre-resolved account scope installed, so
// no database is needed.
func accountCtx(t *testing.T, userId string, scope *accountScope) context.Context {
	t.Helper()
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.Role("writer"),
	})
	memo := &accountScopeMemo{}
	memo.once.Do(func() { memo.scope = scope })
	return context.WithValue(ctx, accountScopeMemoKey{}, memo)
}

func accountScopeWith(accounts ...string) *accountScope {
	s := &accountScope{accounts: map[string]struct{}{}}
	for _, a := range accounts {
		addAccountSpellings(s.accounts, a)
	}
	return s
}

func rowPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fixture payload: %v", err)
	}
	return encoded
}

// ---------------------------------------------------------------------
// The row gate

func TestAccountGrantAdmitsAMemberOfTheRowsAccount(t *testing.T) {
	decl := accountFixture(t, declaredAccountConcept, "accountId")
	ctx := accountCtx(t, "u-member", accountScopeWith("v1:accounts:account:acme"))

	acme := rowPayload(t, map[string]any{"ownerUserId": "u-someone-else", "accountId": "v1:accounts:account:acme"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-1", acme); got != rowAuthzAdmit {
		t.Fatalf("a member of Acme reading Acme's row: got %v, want admit", got)
	}

	// The whole point of the grant, stated as an assertion: the row is
	// owned by somebody else and is admitted anyway.
	beta := rowPayload(t, map[string]any{"ownerUserId": "u-someone-else", "accountId": "v1:accounts:account:beta"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-2", beta); got != rowAuthzDeny {
		t.Fatalf("a member of Acme reading Beta's row: got %v, want deny", got)
	}
	_ = decl
}

func TestAccountGrantAdmitsBothIdSpellings(t *testing.T) {
	accountFixture(t, declaredAccountConcept, "accountId")
	// The scope was resolved from a BARE id; the row stores the canonical
	// one, because an account field is an outgoing @relationship and is
	// stored canonicalized. addAccountSpellings is what makes the two meet.
	ctx := accountCtx(t, "u-member", accountScopeWith("acme"))
	row := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": "v1:accounts:account:acme"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-1", row); got != rowAuthzAdmit {
		t.Fatalf("bare scope against a canonical row value: got %v, want admit", got)
	}
}

func TestAccountGrantHandlesAListField(t *testing.T) {
	accountFixture(t, declaredAccountListConcept, "accountIds")
	ctx := accountCtx(t, "u-member", accountScopeWith("v1:accounts:account:acme"))

	both := rowPayload(t, map[string]any{
		"ownerUserId": "u-other",
		"accountIds":  []any{"v1:accounts:account:beta", "v1:accounts:account:acme"},
	})
	if got := rowAuthzAdmits(ctx, declaredAccountListConcept, "row-1", both); got != rowAuthzAdmit {
		t.Fatalf("a list field containing Acme: got %v, want admit", got)
	}
	neither := rowPayload(t, map[string]any{
		"ownerUserId": "u-other",
		"accountIds":  []any{"v1:accounts:account:beta"},
	})
	if got := rowAuthzAdmits(ctx, declaredAccountListConcept, "row-2", neither); got != rowAuthzDeny {
		t.Fatalf("a list field without Acme: got %v, want deny", got)
	}
}

func TestAccountGrantAdmitsWritesOnTheSameTerms(t *testing.T) {
	// Design D4: a member may WRITE a tied row whatever the owner's rank,
	// if their role holds the verb. The verb is decided upstream, so what
	// this asserts is that the row gate does not narrow writes the way the
	// rank branch does.
	accountFixture(t, declaredAccountConcept, "accountId")
	ctx := accountCtx(t, "u-member", accountScopeWith("v1:accounts:account:acme"))
	row := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": "v1:accounts:account:acme"})
	if got := rowAuthzAdmitsWrite(ctx, declaredAccountConcept, "row-1", row); got != rowAuthzAdmit {
		t.Fatalf("a member writing a tied row: got %v, want admit", got)
	}
}

func TestAccountGrantMemberOfNothingIsAdmittedNothing(t *testing.T) {
	accountFixture(t, declaredAccountConcept, "accountId")
	ctx := accountCtx(t, "u-member", accountScopeWith())
	row := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": "v1:accounts:account:acme"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-1", row); got != rowAuthzDeny {
		t.Fatalf("an empty scope: got %v, want deny", got)
	}
	// ...and the owner branch still works, which is what "OR-ed, not
	// substituted" means.
	own := rowPayload(t, map[string]any{"ownerUserId": "u-member", "accountId": "v1:accounts:account:beta"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-2", own); got != rowAuthzAdmit {
		t.Fatalf("an empty scope reading the caller's OWN row: got %v, want admit", got)
	}
}

func TestAccountGrantStaffReadEveryTiedRowAndNoUntiedOne(t *testing.T) {
	accountFixture(t, declaredAccountConcept, "accountId")
	staff := accountCtx(t, "u-dev", &accountScope{accounts: map[string]struct{}{}, everyAccount: true})

	for _, account := range []string{"v1:accounts:account:acme", "v1:accounts:account:beta"} {
		row := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": account})
		if got := rowAuthzAdmits(staff, declaredAccountConcept, "row", row); got != rowAuthzAdmit {
			t.Fatalf("staff reading %s: got %v, want admit", account, got)
		}
	}
	// The staff rule is "every TIED row", not "every row". A row belonging
	// to no client is somebody's own work and stays owner-only.
	untied := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": ""})
	if got := rowAuthzAdmits(staff, declaredAccountConcept, "row-untied", untied); got != rowAuthzDeny {
		t.Fatalf("staff reading an UNTIED peer row: got %v, want deny", got)
	}
	missing := rowPayload(t, map[string]any{"ownerUserId": "u-other"})
	if got := rowAuthzAdmits(staff, declaredAccountConcept, "row-missing", missing); got != rowAuthzDeny {
		t.Fatalf("staff reading a row with no account field: got %v, want deny", got)
	}
}

func TestAccountGrantWithoutAResolutionWithholdsRatherThanWidens(t *testing.T) {
	// No memo installed at all -- the shape a code path that skipped the
	// entry points would produce. The branch is a disjunct, so declining to
	// widen is the safe direction and the one it must take.
	accountFixture(t, declaredAccountConcept, "accountId")
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-member", Role: auth.Role("writer")})
	row := rowPayload(t, map[string]any{"ownerUserId": "u-other", "accountId": "v1:accounts:account:acme"})
	if got := rowAuthzAdmits(ctx, declaredAccountConcept, "row-1", row); got != rowAuthzDeny {
		t.Fatalf("no resolution installed: got %v, want deny", got)
	}
}

// ---------------------------------------------------------------------
// Rendering and lowering

func TestOrAccountScopeIsOredNotSubstituted(t *testing.T) {
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", Account: "accountId"}
	expr, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("rowAuthzPredicateExpr: %v", err)
	}
	if !treeHasAccountScope(expr) {
		t.Fatal("the rendered predicate carries no account node")
	}
	logical, ok := expr.(*LogicalExpression)
	if !ok || logical.Op != LogicalOr {
		t.Fatalf("the account node replaced the owner comparison rather than being OR-ed onto it: %T", expr)
	}
	// The owner half must survive: it is what keeps "your own rows" true
	// for a caller whose memberships cannot be resolved.
	if treeHasAccountScope(logical.Left) {
		t.Fatal("the left branch is the account node; the owner comparison was lost")
	}
}

func TestAccountScopeIsAbsentWhenNotDeclared(t *testing.T) {
	// The negative control for the test above: a decl WITHOUT the argument
	// must render no account node, or every assertion that one is present
	// is measuring a node that is always there.
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true}
	expr, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("rowAuthzPredicateExpr: %v", err)
	}
	if treeHasAccountScope(expr) {
		t.Fatal("a declaration with no account argument rendered an account node")
	}
}

func TestAccountScopeLowersToTheThreeCases(t *testing.T) {
	eng := &MemQLEngine{}
	node := &AccountScopeExpression{Field: "accountId"}

	empty := eng.accountScopeComparison(accountCtx(t, "u", accountScopeWith()), node)
	if c, ok := empty.(*constantBoolExpression); !ok || c.value {
		t.Fatalf("an empty scope lowered to %T, want a false constant", empty)
	}

	set := eng.accountScopeComparison(accountCtx(t, "u", accountScopeWith("acme")), node)
	match, ok := set.(*accountScopeMatch)
	if !ok {
		t.Fatalf("a set lowered to %T, want an accountScopeMatch", set)
	}
	if match.everyAccount || len(match.accounts) == 0 {
		t.Fatalf("a set lowered to %+v", match)
	}
	// Sorted, so the compiled parameter list and the plan-cache signature
	// are stable rather than following Go's map iteration order.
	for i := 1; i < len(match.accounts); i++ {
		if match.accounts[i-1] > match.accounts[i] {
			t.Fatalf("lowered accounts are not sorted: %v", match.accounts)
		}
	}

	staffCtx := accountCtx(t, "u", &accountScope{accounts: map[string]struct{}{}, everyAccount: true})
	staff := eng.accountScopeComparison(staffCtx, node)
	if m, ok := staff.(*accountScopeMatch); !ok || !m.everyAccount {
		t.Fatalf("staff lowered to %+v", staff)
	}
}

func TestAccountScopeSqlAndPostFilterAgree(t *testing.T) {
	// They must, and the failure if they do not is quiet: a paginated read
	// whose SQL admitted a row and whose post-filter dropped it looks
	// exactly like exhaustion.
	eng := &MemQLEngine{}
	cases := []struct {
		name  string
		value any
		scope *accountScope
		want  bool
	}{
		{"scalar hit", "acme", accountScopeWith("acme"), true},
		{"scalar miss", "beta", accountScopeWith("acme"), false},
		{"list hit", []any{"beta", "acme"}, accountScopeWith("acme"), true},
		{"list miss", []any{"beta"}, accountScopeWith("acme"), false},
		{"empty scalar", "", accountScopeWith("acme"), false},
		{"empty list", []any{}, accountScopeWith("acme"), false},
		{"staff on a tied row", "acme", &accountScope{accounts: map[string]struct{}{}, everyAccount: true}, true},
		{"staff on an untied row", "", &accountScope{accounts: map[string]struct{}{}, everyAccount: true}, false},
		{"staff on an empty list", []any{}, &accountScope{accounts: map[string]struct{}{}, everyAccount: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lowered := eng.accountScopeComparison(accountCtx(t, "u", tc.scope), &AccountScopeExpression{Field: "accountId"})
			node := memorynodes.MemoryNode{
				ID:      "row-1",
				Concept: declaredAccountConcept,
				Payload: rowPayload(t, map[string]any{"accountId": tc.value}),
			}
			var got bool
			switch n := lowered.(type) {
			case *constantBoolExpression:
				got = n.value
			case *accountScopeMatch:
				got = accountScopeMatchesNode(node, n, map[string]map[string]any{})
			default:
				t.Fatalf("unexpected lowered node %T", lowered)
			}
			if got != tc.want {
				t.Fatalf("post-filter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccountFingerprintChangesWithMembership(t *testing.T) {
	// The plan-key property: adding somebody to a group changes what they
	// may see without changing who they are, so the fingerprint must move.
	before := accountScopeWith("acme")
	after := accountScopeWith("acme", "beta")
	a := fingerprintAccountSet("u-1", before.accounts)
	b := fingerprintAccountSet("u-1", after.accounts)
	if a == b {
		t.Fatal("the fingerprint did not change when the account set grew")
	}
	// Two DIFFERENT actors holding the same set must not share a key
	// either: the resolution is per actor, and a key built from the set
	// alone would be correct today and wrong the moment it grows a term.
	if fingerprintAccountSet("u-1", before.accounts) == fingerprintAccountSet("u-2", before.accounts) {
		t.Fatal("two actors with the same account set share a fingerprint")
	}
	// Stable for one actor and one set, or the plan cache never hits.
	if fingerprintAccountSet("u-1", before.accounts) != fingerprintAccountSet("u-1", accountScopeWith("acme").accounts) {
		t.Fatal("the fingerprint is not stable for one actor and one set")
	}
}

// closedDatabase is a handle every read on fails, as a database that refuses
// does. No server is involved: the pool is closed before it is used.
func closedDatabase(t *testing.T) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector()), pgdialect.New())
	if err := db.Close(); err != nil {
		t.Fatalf("closing the pool: %v", err)
	}
	return db
}

// TestAFailedMembershipReadIsNotedForWhoeverAsked (access clarity review, item
// 1): the two membership reads answer from what they could read -- nothing,
// when the database refuses -- because the action guards must not lose
// everybody's access to a blink, and they note the failure on the context's
// auth.ReadFailures, so an operator's read of somebody refuses rather than
// answering as if the person were in no group at all.
func TestAFailedMembershipReadIsNotedForWhoeverAsked(t *testing.T) {
	db := closedDatabase(t)

	if got := activeGroupIdsForUser(context.Background(), db, "u1"); len(got) != 0 {
		t.Fatalf("with nobody listening the failed read answered %v, want no groups", got)
	}
	ctx, failures := auth.ContextWithReadFailures(context.Background())
	if got := activeGroupIdsForUser(ctx, db, "u1"); len(got) != 0 {
		t.Fatalf("a holder changed the answer to %v; it may only listen", got)
	}
	if failures.Err() == nil {
		t.Fatal("the failed membership read was not noted")
	}

	ctx, failures = auth.ContextWithReadFailures(context.Background())
	if groups, accounts, _ := activeGroupsAndAccounts(ctx, db, []string{"g1"}); len(groups)+len(accounts) != 0 {
		t.Fatalf("the failed group read answered groups %v and accounts %v, want none", groups, accounts)
	}
	if failures.Err() == nil {
		t.Fatal("the failed group read was not noted")
	}
}

// TestAPersonsMembershipsAreReadAnewForThem (access clarity review, items 1
// and 3): ContextForPerson puts a named person on an account-scope memo of
// their own, and the per-user membership cache lives in that memo, so their
// memberships are read anew -- under whatever holder is listening -- rather
// than served from what the request read before, with nobody listening.
func TestAPersonsMembershipsAreReadAnewForThem(t *testing.T) {
	e := &MemQLEngine{db: closedDatabase(t)}
	request := contextWithAccountScopeMemo(context.Background(), e)
	memo, _ := request.Value(accountScopeMemoKey{}).(*accountScopeMemo)
	earlier := memo.membershipsEntry("person")
	earlier.once.Do(func() { earlier.m = memberships{groupIds: []string{"g-earlier"}} })

	ctx, failures := auth.ContextWithReadFailures(request)
	person := e.ContextForPerson(ctx, "person", auth.RoleWriter)
	if ac, _ := auth.AccessFromContext(person); ac == nil || ac.UserId != "person" || ac.Role != auth.RoleWriter {
		t.Fatalf("the person's context carries %+v, want their id and role", ac)
	}
	if got := e.membershipsFor(person, "person").groupIds; len(got) != 0 {
		t.Fatalf("the person's memberships were served from the request's memo: %v", got)
	}
	if failures.Err() == nil {
		t.Fatal("the person's membership read was not made under the holder")
	}
	if got := e.membershipsFor(request, "person").groupIds; len(got) != 1 || got[0] != "g-earlier" {
		t.Fatalf("the person's read changed the request's own memo: %v", got)
	}
}
