package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE RANK RULES (epic memql#4832, task memql#4834).
//
// These run against a FIXTURE concept for the reason the composite tier's
// tests do, and one more. The reason they share: registering a fixture is how
// a gate is measured resolving a tier from the REAL registry rather than from
// a decl handed straight to the function under test. The reason particular to
// this file: the rank rules are the first tier arguments that change what an
// existing concept's rows are worth, so pointing them at a live concept would
// make a failure here indistinguishable from a failure in that concept's own
// declaration.
const declaredRankConcept = "v1:rowauthzfixture:rank"

// rankFixture registers a concept declaring the rank modifiers.
//
// The `unowned` floor is `admin` -- rank 200 in the base ladder -- so the
// tests below can distinguish four outcomes with one fixture: below the floor,
// at it, above it, and unowned-vs-owned.
func rankFixture(t *testing.T, strict bool, unowned string) *langparser.RowAuthzDecl {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	decl := &langparser.RowAuthzDecl{
		Tier:        langparser.RowAuthzOwned,
		Owner:       "ownerUserId",
		RankVisible: true,
		RankStrict:  strict,
		Unowned:     unowned,
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		declaredRankConcept: {
			Name:     declaredRankConcept,
			NodeType: "rank",
			RowAuthz: decl,
		},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROL, and it earns its place here more than anywhere:
	// an undeclared concept admits everything, so without this every
	// assertion below could pass while measuring nothing at all.
	got := rowAuthzDeclFor(declaredRankConcept)
	if got == nil {
		t.Fatal("the rank fixture is not in the registry, so every assertion below would measure " +
			"an UNDECLARED concept -- which admits everything and passes for the wrong reason")
	}
	if !got.RankVisible {
		t.Fatalf("the fixture resolved to %+v, which carries no rank branch", *got)
	}
	return got
}

// baseLadder is the compiled fallback ladder, which is what rankOf resolves
// against when no role catalog is readable -- the shape every test in this
// file runs under, since none of them has a database.
func baseLadder() roleLadder { return roleLadder{ranks: map[string]int{}} }

// ---------------------------------------------------------------------
// The floor, and the spelling that fails OPEN
// ---------------------------------------------------------------------

// TestAnUnresolvableFloorAdmitsNobody is the highest-value test in this file.
//
// The natural spelling of a rank floor -- `actorRank >= ladder.rankOf(slug)`
// -- reads correctly and is a security hole: rankOf answers 0 for a slug it
// does not know, and EVERY rank clears 0. So one typo in
// `unowned="devleoper"` silently converts a gate into a pass-through that
// hands every deployment-owned row to every caller, with the declaration still
// reading like a gate to anyone reviewing it.
//
// This asserts the fail-CLOSED direction, and the case below it asserts the
// instrument can move at all -- a fail-closed test that would also pass
// against a function returning false unconditionally is not evidence.
func TestAnUnresolvableFloorAdmitsNobody(t *testing.T) {
	ladder := baseLadder()
	for _, slug := range []string{"devleoper", "", "nosuchrole", "OWNER-ish"} {
		for _, rank := range []int{0, 50, 100, 200, 300, 400, 9000} {
			if rankFloorAdmits(ladder, slug, rank) {
				t.Fatalf("rankFloorAdmits(%q, actorRank=%d) admitted. An unresolvable floor ranks 0 "+
					"and every rank clears 0, so this would hand every cluster-owned row to every "+
					"caller while the declaration still read like a gate", slug, rank)
			}
		}
	}
}

// The reachable positive for the test above: a floor that DOES resolve admits
// at and above itself, and refuses below.
func TestAResolvableFloorAdmitsAtAndAboveItself(t *testing.T) {
	ladder := baseLadder()
	cases := []struct {
		slug  string
		rank  int
		admit bool
	}{
		{"admin", 400, true},  // owner clears the admin floor
		{"admin", 300, true},  // developer clears it -- the flipped ladder
		{"admin", 200, true},  // admin is AT the floor
		{"admin", 100, false}, // writer/user is below
		{"admin", 50, false},  // reader/viewer is below
		{"owner", 400, true},
		{"owner", 300, false}, // developer does NOT clear an owner floor
	}
	for _, c := range cases {
		if got := rankFloorAdmits(ladder, c.slug, c.rank); got != c.admit {
			t.Fatalf("rankFloorAdmits(%q, actorRank=%d) = %v, want %v", c.slug, c.rank, got, c.admit)
		}
	}
}

// ---------------------------------------------------------------------
// The predicate the tier injects
// ---------------------------------------------------------------------

// The rank branch is OR-ED onto the rendered tier predicate, never
// substituted for it. Both halves are load-bearing: the owner comparison keeps
// "your own rows" true for a caller the principal table cannot rank, and the
// rank branch adds everybody below them.
func TestRankTierOrsTheRankBranchOntoTheOwnerTerm(t *testing.T) {
	decl := rankFixture(t, false, "admin")
	expr, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("rowAuthzPredicateExpr: %v", err)
	}
	logical, ok := expr.(*LogicalExpression)
	if !ok || logical.Op != LogicalOr {
		t.Fatalf("the rank tier rendered %T, want a disjunction carrying the owner term and the rank branch", expr)
	}
	if !treeHasRankScope(logical) {
		t.Fatal("the injected term carries no rank node, so the rank branch would never be resolved")
	}
	// The owner half must survive. Losing it is the silent failure: a
	// caller the ladder cannot rank would lose access to their OWN rows,
	// and every test that seeds a rankable owner would still pass.
	if treeHasRankScope(logical.Left) {
		t.Fatal("the rank node replaced the owner term rather than joining it; a caller the " +
			"principal table cannot rank must keep reading their own rows")
	}
}

// A concept declaring NO rank modifier must render exactly what it rendered
// before this epic. This is the migration claim -- "~130 tier declarations
// exist and none changes meaning" -- as a test rather than an assurance.
func TestATierWithoutRankModifiersIsUnchanged(t *testing.T) {
	for _, decl := range []*langparser.RowAuthzDecl{
		{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
		{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true},
		{Tier: langparser.RowAuthzClusterOwner},
		{Tier: langparser.RowAuthzPublic},
	} {
		expr, err := rowAuthzPredicateExpr(decl)
		if err != nil {
			t.Fatalf("rowAuthzPredicateExpr(%+v): %v", decl, err)
		}
		if expr != nil && treeHasRankScope(expr) {
			t.Fatalf("%+v rendered a rank node; a declaration that does not ask for rank must not get it", decl)
		}
	}
}

// ---------------------------------------------------------------------
// The row gate
// ---------------------------------------------------------------------

// rankCtx builds a context carrying an actor and a resolved scope, without a
// database: resolveRankScope reads the principal table through the engine, and
// these cases are about the DECISION rather than about the read.
func rankCtx(t *testing.T, role auth.Role, userId string, unranked bool, scope *rankScope) context.Context {
	t.Helper()
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:   userId,
		Role:     role,
		Unranked: unranked,
	})
	memo := &rankScopeMemo{}
	memo.once.Do(func() { memo.scope = scope })
	return context.WithValue(ctx, rankScopeMemoKey{}, memo)
}

func scopeWith(actorRank int, read, write []string) *rankScope {
	s := &rankScope{
		actorRank:   actorRank,
		ladder:      baseLadder(),
		readOwners:  map[string]struct{}{},
		writeOwners: map[string]struct{}{},
	}
	for _, id := range read {
		addOwnerSpellings(s.readOwners, id)
	}
	for _, id := range write {
		addOwnerSpellings(s.writeOwners, id)
	}
	return s
}

// D2 vs D3 in one table: the read rule includes peers, the write rule does
// not. This is the one-word difference between the two decisions, and the
// place a regression would be invisible -- a write path that kept delegating
// to the READ admission would hand every peer write authority the moment a
// concept declared rankVisible.
func TestRankReadsIncludePeersAndRankWritesDoNot(t *testing.T) {
	decl := rankFixture(t, true, "admin")
	// The caller is a developer; `peer` is another developer, `below` is a
	// reader. Reads admit both, writes admit only `below`.
	scope := scopeWith(300, []string{"me", "peer", "below"}, []string{"me", "below"})
	ctx := rankCtx(t, auth.RoleDeveloper, "me", false, scope)

	cases := []struct {
		owner     string
		read      bool
		write     bool
		rationale string
	}{
		{"below", true, true, "a reader's row is readable and writable by a developer"},
		{"peer", true, false, "a PEER's row is readable and READ-ONLY -- D3's whole point"},
		{"stranger", false, false, "an owner outside the scope is neither"},
	}
	for _, c := range cases {
		if got := rankAdmitsRow(ctx, decl, c.owner, true, false); got != c.read {
			t.Fatalf("read of a row owned by %q = %v, want %v (%s)", c.owner, got, c.read, c.rationale)
		}
		if got := rankAdmitsRow(ctx, decl, c.owner, true, true); got != c.write {
			t.Fatalf("write of a row owned by %q = %v, want %v (%s)", c.owner, got, c.write, c.rationale)
		}
	}
}

// rankStrict is what admits a write at all. A concept declaring only
// rankVisible gets the read widening and keeps the owned tier's write rule.
func TestRankWritesNeedTheStrictModifier(t *testing.T) {
	decl := rankFixture(t, false, "")
	scope := scopeWith(300, []string{"below"}, []string{"below"})
	ctx := rankCtx(t, auth.RoleDeveloper, "me", false, scope)
	if !rankAdmitsRow(ctx, decl, "below", true, false) {
		t.Fatal("rankVisible must still admit the READ")
	}
	if rankAdmitsRow(ctx, decl, "below", true, true) {
		t.Fatal("a concept declaring rankVisible but NOT rankStrict must keep the owned tier's " +
			"write rule; granting the write here would make the two modifiers one")
	}
}

// An UNOWNED row (present and empty) is the deployment's. It is admitted at
// the declared floor and refused below it -- and refused for WRITES at every
// rank, because a row nobody owns has no owner to be strictly below.
func TestUnownedRowsAreAdmittedAtTheDeclaredFloorOnly(t *testing.T) {
	decl := rankFixture(t, true, "admin")
	for _, c := range []struct {
		rank  int
		admit bool
	}{{400, true}, {300, true}, {200, true}, {100, false}, {50, false}} {
		ctx := rankCtx(t, auth.RoleDeveloper, "me", false, scopeWith(c.rank, nil, nil))
		if got := rankAdmitsRow(ctx, decl, "", true, false); got != c.admit {
			t.Fatalf("unowned row read at rank %d = %v, want %v", c.rank, got, c.admit)
		}
		if rankAdmitsRow(ctx, decl, "", true, true) {
			t.Fatalf("unowned row WRITE at rank %d was admitted; a row nobody owns has no owner "+
				"to be strictly below, so rank-strict has no answer for it", c.rank)
		}
	}
}

// An ABSENT owner field is a different thing from a present-and-empty one, and
// stays denied at every rank -- matching what the owned tier has always done
// with one: "a row that cannot say who owns it is not a row this caller can be
// shown to own".
func TestAnAbsentOwnerFieldIsNeverAdmittedByRank(t *testing.T) {
	decl := rankFixture(t, true, "admin")
	ctx := rankCtx(t, auth.RoleOwner, "me", false, scopeWith(400, nil, nil))
	if rankAdmitsRow(ctx, decl, "", false, false) {
		t.Fatal("a row with no owner FIELD was admitted by the rank branch. Absent and " +
			"present-and-empty must stay distinguishable: only the second is a deliberate " +
			"statement of cluster ownership")
	}
}

// With no scope resolved, the rank branch declines to widen. It is a DISJUNCT,
// so declining leaves exactly the pre-rank behaviour -- a row withheld, never
// a row disclosed.
func TestWithNoResolvedScopeTheRankBranchWithholds(t *testing.T) {
	decl := rankFixture(t, true, "admin")
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "me", Role: auth.RoleOwner})
	if rankAdmitsRow(ctx, decl, "anyone", true, false) || rankAdmitsRow(ctx, decl, "", true, false) {
		t.Fatal("the rank branch widened with no resolved scope; an unresolvable gate must " +
			"withhold, and it can do so safely here only because it is a disjunct")
	}
}

// ---------------------------------------------------------------------
// D4 -- the actors the rank rules do not govern
// ---------------------------------------------------------------------

// Every non-principal constructor sets Unranked. Without it, a rank-strict
// concept turns every retention sweep and boot seed into a peer-write and
// stops it -- and a sweep that retires nothing looks exactly like a sweep with
// nothing to retire.
func TestEveryNonPrincipalActorIsUnranked(t *testing.T) {
	if ac := auth.MaintenanceActor("auditEventRetentionSweep"); ac == nil || !ac.Unranked {
		t.Fatal("auth.MaintenanceActor is not marked Unranked; every retention sweep on a " +
			"rank-strict concept would be refused as a peer-write")
	}
	ctx := auth.ContextWithUserActor(context.Background(), "someone")
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || !ac.Unranked {
		t.Fatal("auth.ContextWithUserActor is not marked Unranked; borrowed authority carries a " +
			"synthetic RoleWriter, and ranking it would refuse a borrowed admin their own rows")
	}
	// The seed materializer's actor is built inside this package.
	seedCtx := systemActorContext(context.Background())
	seedAc, _ := auth.AccessFromContext(seedCtx)
	if seedAc == nil || !seedAc.Unranked {
		t.Fatal("the seed materializer's actor is not marked Unranked; a boot seed refused as a " +
			"peer-write is a cluster that will not finish starting")
	}
}

// D3 withdraws the cluster-owner WRITE escape on a rank-strict concept -- and
// keeps it for an unranked actor, which is the D4 clause that keeps the
// cluster booting.
func TestRankStrictWithdrawsTheClusterOwnerEscapeExceptForUnrankedActors(t *testing.T) {
	strict := &langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, RankStrict: true,
	}
	plain := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}

	human := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "op", Role: auth.RoleOwner})
	if _, escaped := rowAuthzWriteEscapeFor(human, strict); escaped {
		t.Fatal("a cluster owner still escaped the write guard on a rank-strict concept. " +
			"'Peer rows are read-only INCLUDING owner-to-owner' cannot be true while a blanket " +
			"escape returns before any owner is resolved")
	}
	if _, escaped := rowAuthzWriteEscapeFor(human, plain); !escaped {
		t.Fatal("the cluster-owner escape was withdrawn from a concept that declares no rank " +
			"rule; the withdrawal must be scoped to rankStrict or ~130 declarations change meaning")
	}
	sweep := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("auditEventRetentionSweep"))
	if _, escaped := rowAuthzWriteEscapeFor(sweep, strict); !escaped {
		t.Fatal("an unranked actor lost the escape on a rank-strict concept (D4). Every retention " +
			"sweep and boot seed on that concept would stop, silently")
	}
}

// ---------------------------------------------------------------------
// #4817 -- a non-principal cannot own a row
// ---------------------------------------------------------------------

// The stamp-undo three files claimed and nothing implemented.
func TestANonPrincipalActorDoesNotOwnTheRowItCreates(t *testing.T) {
	rankFixture(t, false, "admin")
	seed := auth.MaintenanceActor("seedSelfAccount")
	ctx := auth.ContextWithAccess(context.Background(), seed)

	payload := map[string]any{"ownerUserId": seed.UserId, "name": "My company"}
	undoNonPrincipalOwnerStamp(ctx, declaredRankConcept, payload)
	if got := payload["ownerUserId"]; got != "" {
		t.Fatalf("ownerUserId = %q after a maintenance-actor create, want the empty string. "+
			"A synthetic id resolves to no principal and therefore to no rank, which makes the "+
			"row's visibility something nothing can reason about", got)
	}
	if _, present := payload["ownerUserId"]; !present {
		t.Fatal("the field was DELETED rather than emptied. Present-and-empty is a statement " +
			"of cluster ownership; absent is a row that never said, and the rank branch reads " +
			"them differently on purpose")
	}
}

// BORROWED AUTHORITY IS NOT SYNTHETIC, and this is the regression test for the
// mistake that separated the two flags.
//
// ContextWithUserActor is Unranked -- its RoleWriter is a stand-in and must
// not be read as a rung -- and it is NOT Synthetic: its UserId is a real
// person's, and a row it creates is genuinely theirs. Keying the owner undo on
// Unranked blanked `ownerUserId` on every row written through it (the worker's
// delegation policy, an app session), leaving them owned by nobody.
//
// It was caught by three db-gated tests and could not have been caught by a
// unit test here, because the stamp only happens on a real write -- which is
// why this asserts the FLAGS, the thing a unit test can see, rather than
// re-staging the write.
func TestBorrowedAuthorityStillOwnsTheRowsItCreates(t *testing.T) {
	rankFixture(t, false, "admin")
	ctx := auth.ContextWithUserActor(context.Background(), "a-real-person")
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		t.Fatal("ContextWithUserActor installed no access context")
	}
	if !ac.Unranked {
		t.Fatal("borrowed authority must stay Unranked: its RoleWriter is a stand-in, and " +
			"ranking it would refuse a borrowed admin their own rows")
	}
	if ac.Synthetic {
		t.Fatal("borrowed authority must NOT be Synthetic. It acts as a real person, so the " +
			"rows it creates are that person's -- marking it synthetic blanks their owner and " +
			"leaves the row owned by nobody")
	}
	payload := map[string]any{"ownerUserId": "a-real-person"}
	undoNonPrincipalOwnerStamp(ctx, declaredRankConcept, payload)
	if payload["ownerUserId"] != "a-real-person" {
		t.Fatalf("ownerUserId = %v after a borrowed-authority create, want the person's own id",
			payload["ownerUserId"])
	}
}

// The three SYNTHETIC constructors carry both flags. Unranked alone would
// leave them subject to the owner undo's counterpart problem -- a boot seed
// owned by `system:seedMaterializer` is a row whose visibility nothing can
// reason about (memql#4817).
func TestTheSyntheticActorsCarryBothFlags(t *testing.T) {
	for name, ac := range map[string]*auth.AccessContext{
		"MaintenanceActor":      auth.MaintenanceActor("seedSelfAccount"),
		"the seed materializer": mustAccess(t, systemActorContext(context.Background())),
	} {
		if ac == nil {
			t.Fatalf("%s built no access context", name)
		}
		if !ac.Unranked || !ac.Synthetic {
			t.Fatalf("%s: Unranked=%v Synthetic=%v -- a synthetic actor is both, and the two "+
				"say different things (see AccessContext.Synthetic)", name, ac.Unranked, ac.Synthetic)
		}
	}
}

func mustAccess(t *testing.T, ctx context.Context) *auth.AccessContext {
	t.Helper()
	ac, _ := auth.AccessFromContext(ctx)
	return ac
}

// It only ever deletes the actor's OWN id -- so a system actor provisioning a
// row FOR a user leaves that row theirs. Without this the same rule would
// quietly un-own every row any automation ever created on somebody's behalf.
func TestTheOwnerUndoNeverTouchesAThirdPartysRow(t *testing.T) {
	rankFixture(t, false, "admin")
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("seedSelfAccount"))
	payload := map[string]any{"ownerUserId": "a-real-user"}
	undoNonPrincipalOwnerStamp(ctx, declaredRankConcept, payload)
	if payload["ownerUserId"] != "a-real-user" {
		t.Fatalf("ownerUserId = %v; a system actor provisioning a row FOR a user must leave it theirs",
			payload["ownerUserId"])
	}
}

// A real principal's create is untouched -- the reachable positive for the two
// tests above.
func TestAPrincipalsOwnStampSurvives(t *testing.T) {
	rankFixture(t, false, "admin")
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: auth.RoleWriter})
	payload := map[string]any{"ownerUserId": "u1"}
	undoNonPrincipalOwnerStamp(ctx, declaredRankConcept, payload)
	if payload["ownerUserId"] != "u1" {
		t.Fatalf("ownerUserId = %v; an ordinary caller's own stamp must survive", payload["ownerUserId"])
	}
}

// ---------------------------------------------------------------------
// The self account (memql#4837)
// ---------------------------------------------------------------------

// Archive is the only exit in this model -- no unarchive, no delete -- so
// archiving the self account is a one-way trip to a cluster with no company.
func TestTheSelfAccountCannotBeArchived(t *testing.T) {
	e := &MemQLEngine{}
	ctx := context.Background()
	if err := e.validateSelfAccountNotArchived(ctx, selfAccountId, map[string]any{"status": "archived"}); err == nil {
		t.Fatal("archiving the self account was allowed")
	}
	// Every other edit stays legal, including on the self row: a locked
	// field would make a first-run typo permanent, and there is no delete.
	if err := e.validateSelfAccountNotArchived(ctx, selfAccountId, map[string]any{"status": "active", "name": "Renamed"}); err != nil {
		t.Fatalf("a non-archiving edit to the self account was refused: %v", err)
	}
	// An ordinary client account archives exactly as before.
	if err := e.validateSelfAccountNotArchived(ctx, "v1:accounts:account:acme", map[string]any{"status": "archived"}); err != nil {
		t.Fatalf("archiving an ordinary account was refused: %v", err)
	}
}

// TestTheRankFloorSurvivesRegistration is a regression test for a bug that
// shipped a gate which parsed, validated and gated NOTHING.
//
// The registry hands out CLONES (baseregistry stores a copy function), and
// Function.clone() lists its fields by name. `RequiresRank` was resolved
// correctly by the loader and read correctly by the enforcement, and was empty
// at every call site, because the one function in between did not name it.
//
// The class is worth a test rather than a comment: every future field on
// Function has the same failure mode, and it is silent in the safe-looking
// direction -- an authorization floor that admits everyone.
func TestTheRankFloorSurvivesRegistration(t *testing.T) {
	registry := newFunctionRegistry()
	if err := registry.Upsert(&Function{
		Name:         "probeFlooredConstruct",
		FunctionKind: "query",
		Enabled:      true,
		RequiresRank: "admin",
		ServerOnly:   true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := registry.Lookup("probeFlooredConstruct")
	if !ok || got == nil {
		t.Fatal("the construct did not register")
	}
	if got.RequiresRank != "admin" {
		t.Fatalf("RequiresRank = %q after a registry round trip, want \"admin\".\n"+
			"Function.clone() lists its fields by name, so a field it does not mention is a "+
			"field every registered construct loses -- and for this one that means an "+
			"authorization floor that admits everybody, with the annotation still in the DSL "+
			"and the load-time validation still passing.", got.RequiresRank)
	}
	// The neighbour, as a control: if BOTH are empty the registry itself is
	// broken and this test is measuring that instead.
	if !got.ServerOnly {
		t.Fatal("ServerOnly was also lost, so this test is measuring a broken registry rather " +
			"than a missing field in clone()")
	}
}

// THE COMPOSITE BYPASS MUST NOT HAND BACK THE WRITE ESCAPE rankStrict just
// withdrew.
//
// `@rowAuthz(owner="f", rankVisible, rankStrict, clusterOwner)` is a
// declaration the parser accepts and the formatter renders, and the two halves
// pull in opposite directions: rowAuthzWriteEscapeFor defers for a cluster
// owner, and the composite arm inside rowAuthzAdmits would then admit them
// anyway -- before the owner comparison and before rankAdmitsRow. D3's "peer
// rows are read-only, owner-to-owner included" would be decorative for exactly
// the concepts that asked for it.
//
// Latent, which is why it needs a test rather than a comment: no concept
// declares rankStrict today, so the first one to adopt it would silently not
// get the guarantee.
func TestTheCompositeBypassDoesNotUndoRankStrictOnWrites(t *testing.T) {
	decl := &langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzOwned, Owner: "ownerUserId",
		RankVisible: true, RankStrict: true, ClusterOwnerBypass: true,
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		declaredRankConcept: {Name: declaredRankConcept, NodeType: "rank", RowAuthz: decl},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })
	if got := rowAuthzDeclFor(declaredRankConcept); got == nil || !got.RankStrict || !got.ClusterOwnerBypass {
		t.Fatal("the fixture did not land; this test would measure an undeclared concept")
	}

	// An owner, and a row owned by a PEER owner.
	scope := scopeWith(400, []string{"me", "peer"}, []string{"me"})
	ctx := rankCtx(t, auth.RoleOwner, "me", false, scope)
	payload := []byte(`{"ownerUserId":"peer"}`)

	if got := rowAuthzAdmitsWrite(ctx, declaredRankConcept, "row-1", payload); got == rowAuthzAdmit {
		t.Fatal("a cluster owner was admitted to WRITE a peer owner's row on a rankStrict " +
			"concept. rowAuthzWriteEscapeFor withdraws the blanket escape for exactly this " +
			"case, and the composite arm handed it straight back one layer down")
	}
	// READS are untouched -- reading across users is what the composite is for.
	if got := rowAuthzAdmits(ctx, declaredRankConcept, "row-1", payload); got != rowAuthzAdmit {
		t.Fatalf("the cluster owner's READ was %v; the composite must still admit it", got)
	}
	// And a row BELOW them is still writable, so this is a peer rule rather
	// than a blanket refusal.
	below := []byte(`{"ownerUserId":"me"}`)
	if got := rowAuthzAdmitsWrite(ctx, declaredRankConcept, "row-2", below); got != rowAuthzAdmit {
		t.Fatalf("the caller's OWN row was refused a write (%v)", got)
	}
	// An UNRANKED actor keeps the escape (D4), or every sweep stops.
	sweepCtx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("seedSelfAccount"))
	if _, escaped := rowAuthzWriteEscapeFor(sweepCtx, decl); !escaped {
		t.Fatal("an unranked actor lost the escape on a rankStrict+clusterOwner concept")
	}
}

// ---------------------------------------------------------------------
// Subscriptions (D7 -- the tier covers reads, writes AND live events)
// ---------------------------------------------------------------------

// TestSubscriptionsInheritTheRankBranch is the third of the three surfaces
// #4834 names, and it is the one whose absence would be least visible.
//
// A `rankVisible` concept that served a peer's row on a READ and dropped the
// live event for the SAME row would be correct on load and frozen after --
// the exact shape clients/os/README.md warns about by name. Nothing would
// error; the list would simply stop moving.
//
// It also pins the property that makes that work: the fan-out must run under
// a context carrying a resolved scope. Without one the rank branch declines to
// widen -- the safe direction and the wrong answer here -- so this asserts
// BOTH directions rather than only the admitting one.
func TestSubscriptionsInheritTheRankBranch(t *testing.T) {
	rankFixture(t, false, "admin")
	payload := []byte(`{"ownerUserId":"below","name":"a peer's row"}`)

	actor := &auth.AccessContext{UserId: "me", Role: auth.RoleDeveloper}
	scope := scopeWith(300, []string{"me", "below"}, []string{"me"})

	// WITH a resolved scope: the event reaches the stream, exactly as the read
	// path returns the row.
	withScope := rankCtx(t, auth.RoleDeveloper, "me", false, scope)
	if got := AdmitSubscriptionRow(withScope, actor, declaredRankConcept, "row-1", payload); got != SubscriptionAdmit {
		t.Fatalf("a rank-visible row was %s at fan-out but is readable through the read path. "+
			"A concept whose list is correct on load and frozen afterwards is the failure this "+
			"case exists for", got)
	}

	// WITHOUT one: withheld rather than served. This is the direction that
	// must never invert -- a fan-out that widened on an unresolved scope would
	// hand out rows the read path refuses.
	bare := auth.ContextWithAccess(context.Background(), actor)
	if got := AdmitSubscriptionRow(bare, actor, declaredRankConcept, "row-1", payload); got == SubscriptionAdmit {
		t.Fatal("a rank-visible row was ADMITTED at fan-out with no resolved scope. The rank " +
			"branch is a disjunct and must decline to widen when it cannot answer; admitting " +
			"here would serve rows the read path refuses")
	}

	// A row above the caller's rank is refused with a scope in hand, which is
	// the reachable negative for the first case: without it, an implementation
	// that admitted everything would pass.
	above := []byte(`{"ownerUserId":"stranger","name":"an owner's row"}`)
	if got := AdmitSubscriptionRow(withScope, actor, declaredRankConcept, "row-2", above); got == SubscriptionAdmit {
		t.Fatal("a row owned outside the caller's scope was admitted at fan-out")
	}
}
