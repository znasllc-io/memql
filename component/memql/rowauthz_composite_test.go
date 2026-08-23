package memql

import (
	"context"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The COMPOSITE tier, `@rowAuthz(owner="<field>", clusterOwner)` -- "the
// owner, or a cluster owner" (memql#4312).
//
// Why it had to exist: an `owner=` tier has NO cluster-owner bypass, so
// declaring a live operator surface plain-owned would hide every other
// user's rows from the operator console -- which is the wrong trade, and
// the likely reason so much of the undeclared long tail stayed
// undeclared.
//
// # These tests run against a FIXTURE concept, and that is deliberate
//
// No concept in the tree declares the composite tier yet. The five the
// design proposed (memql#4313: plan, task, taskState, worker
// registration, auditEvent) are all blocked on the same prerequisite --
// the engine's own internal reads over them run under an actor carrying
// no AccessContext, which an owned floor REFUSES -- so declaring any of
// them here would break the planner loop, the retention sweep or the
// worker dispatcher rather than measure this tier. Registering a fixture
// is the same choice the design already made for the `granted` tier,
// which no concept declares either ("that is deliberate (D3) -- a future
// granted concept must not make the live band die silently").
//
// The fixture is registered into the real registry, so every gate under
// test resolves it exactly as it resolves a tree concept -- these are not
// hand-built decls handed straight to the function under test.
const declaredCompositeConcept = "v1:rowauthzfixture:composite"

// compositeFixture registers a concept declaring the composite tier and
// removes it again when the test ends.
//
// Registered AFTER LoadUnifiedConcepts, because that call replaces the
// registry wholesale -- seeding first would leave the fixture silently
// absent and every assertion below measuring an undeclared concept, which
// is the failure mode where a security test passes by admitting
// everything.
func compositeFixture(t *testing.T) *langparser.RowAuthzDecl {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	decl := &langparser.RowAuthzDecl{
		Tier:               langparser.RowAuthzOwned,
		Owner:              "ownerUserId",
		ClusterOwnerBypass: true,
	}
	// Snapshot-and-restore rather than a targeted delete: the registry
	// exposes Remove only on the concrete type, and restoring the whole
	// map cannot leave the fixture behind on a path that panics past a
	// single delete.
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		declaredCompositeConcept: {
			Name:     declaredCompositeConcept,
			NodeType: "composite",
			RowAuthz: decl,
		},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROL. If the fixture did not land, rowAuthzDeclFor
	// answers nil, every gate below admits unconditionally, and all of
	// these tests pass while measuring nothing.
	got := rowAuthzDeclFor(declaredCompositeConcept)
	if got == nil {
		t.Fatal("the composite fixture is not in the registry, so every assertion below would " +
			"measure an UNDECLARED concept -- which admits everything and passes for the wrong reason")
	}
	if got.Tier != langparser.RowAuthzOwned || !got.ClusterOwnerBypass {
		t.Fatalf("the fixture resolved to %+v, not the composite tier", *got)
	}
	return got
}

// The declaration parses to the owned tier CARRYING the bypass, not to a
// fifth tier. Every site that resolves an owner field switches on
// Tier == RowAuthzOwned -- the loader's owner-property validation, the
// insert stamp, the actorless-read refusal, the conformance classifier --
// and a new tier value would have fallen silently out of all four.
func TestCompositeTierIsTheOwnedTierCarryingTheBypass(t *testing.T) {
	decl := compositeFixture(t)
	if decl.Tier != langparser.RowAuthzOwned {
		t.Fatalf("%s declares tier %q; the composite must remain the OWNED tier so the owner-field "+
			"machinery keeps working", declaredCompositeConcept, decl.Tier)
	}
	if !decl.ClusterOwnerBypass {
		t.Fatalf("%s does not carry the cluster-owner bypass; this fixture measures nothing",
			declaredCompositeConcept)
	}
	if strings.TrimSpace(decl.Owner) == "" {
		t.Fatal("the composite declaration names no owner field")
	}
}

// The injected predicate is the OR of the two branches, and it is a
// predicate the engine's own parser accepts -- the term enforced is
// literally the term rendered, which is what keeps a renderer and a
// matcher from drifting apart about the thing they both describe.
func TestCompositeTierInjectsTheOwnerOrAdminPredicate(t *testing.T) {
	decl := compositeFixture(t)
	rendered := InjectedPredicate(decl)

	for _, want := range []string{decl.Owner + "==actor.userId", "actor.isClusterOwner==true", "||"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("InjectedPredicate(composite) = %q, want it to contain %q", rendered, want)
		}
	}

	// It must PARSE, or every read over the concept refuses rather than
	// narrowing -- rowAuthzPredicateExpr turns an unusable declaration
	// into an error on purpose.
	expr, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("the composite predicate %q does not build: %v", rendered, err)
	}
	if expr == nil {
		t.Fatal("the composite predicate built to nothing; only the public tier injects nothing")
	}
	if _, isLogical := expr.(*LogicalExpression); !isLogical {
		t.Fatalf("the composite predicate built to %T, want a disjunction", expr)
	}
}

// Row admission admits on EITHER branch. This is the half that covers a
// raw client query string and graph expansion, neither of which has a
// filter to AND anything into.
func TestCompositeTierRowGateAdmitsOwnerAndClusterOwner(t *testing.T) {
	decl := compositeFixture(t)
	mine := rowOf(t, declaredCompositeConcept, declaredCompositeConcept+":mine",
		map[string]any{decl.Owner: "user-a", "label": "mine"})

	if !admitRowAuthzNode(callerCtx("user-a"), mine) {
		t.Error("the owner was denied their own row on the composite tier")
	}
	if !admitRowAuthzNode(ownerRoleCtx("root"), mine) {
		t.Error("a CLUSTER OWNER was denied another user's row on the composite tier -- the second " +
			"branch of the declaration is exactly what this tier exists to add, and without it the " +
			"operator console cannot see the rows it is built to operate on")
	}
	if admitRowAuthzNode(callerCtx("user-b"), mine) {
		t.Error("a third user was shown a row they neither own nor administer")
	}
	// memql#2801 / #3172 finding 4: no identity, no rows. The bypass must
	// not turn an absent actor into a cluster owner.
	if admitRowAuthzNode(context.Background(), mine) {
		t.Error("a caller with no access context was admitted by the composite tier")
	}
}

// A row that cannot say who owns it is still denied to a plain caller,
// and still visible to a cluster owner -- the bypass does not depend on
// the owner field resolving.
func TestCompositeTierClusterOwnerDoesNotDependOnTheOwnerField(t *testing.T) {
	compositeFixture(t)
	headless := rowOf(t, declaredCompositeConcept, declaredCompositeConcept+":headless",
		map[string]any{"label": "no owner key at all"})

	if admitRowAuthzNode(callerCtx("user-a"), headless) {
		t.Error("a row carrying no owner field was admitted to a plain caller")
	}
	if !admitRowAuthzNode(ownerRoleCtx("root"), headless) {
		t.Error("the cluster owner was denied a row whose owner field is absent; the admin branch " +
			"does not read the owner field at all")
	}
}

// THE WRITE GUARD IGNORES THE SECOND ARGUMENT (design section 4.5).
//
// A cluster owner reading a row is not a cluster owner editing it as its
// author, so the composite must not widen the write path. It does not,
// and the reason is worth stating because it is easy to misread: the
// cluster-owner escape a write already has is rowAuthzWriteEscape's, it
// pre-dates this tier, and it applies to EVERY declared tier alike. So
// the property to hold is that the composite grants nothing further --
// a non-owner without that standing escape is refused exactly as the
// plain owned tier refuses them.
func TestCompositeTierDoesNotWidenTheWriteGuard(t *testing.T) {
	decl := compositeFixture(t)
	prior := map[string]any{decl.Owner: "user-a", "label": "user-a's worker"}

	err := guardRowAuthzWrite(callerCtx("user-b"), declaredCompositeConcept,
		declaredCompositeConcept+":mine", prior, true, true)
	if err == nil {
		t.Fatal("the write guard admitted user-b's write onto user-a's row on a composite-declared " +
			"concept. The bypass is a READ-side widening; a write is authorized against the row's " +
			"author and nothing else (memql#3174).")
	}

	// And the owner still writes their own row.
	if err := guardRowAuthzWrite(callerCtx("user-a"), declaredCompositeConcept,
		declaredCompositeConcept+":mine", prior, true, true); err != nil {
		t.Fatalf("the write guard refused the row's own owner: %v", err)
	}
}

// The injected term is CLONED before it reaches a plan, and the concept
// is stamped on every comparison inside it.
//
// Both halves are load-bearing and neither is obvious. The predicate is
// memoised by rendered string, and rewriteFilterFieldRefs mutates the
// injected term in place (turning a bare `requestedBy` into
// `payload.requestedBy`), so handing out the cached node corrupts the
// entry every later query is given. And the owner field is an
// @relationship, so its stored value is canonical
// (`v1:identity:user:u1`) while `actor.userId` resolves to the bare `u1`
// -- an unstamped comparison canonicalizes from
// extractConceptFromExpression, which answers "" here, and the term then
// matches NOTHING, the owner's own rows included (memql#3172).
func TestCompositePredicateIsClonedAndStamped(t *testing.T) {
	decl := compositeFixture(t)

	first, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("rowAuthzPredicateExpr: %v", err)
	}
	second, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("rowAuthzPredicateExpr (second call): %v", err)
	}
	if first == second {
		t.Fatal("two builds of the composite predicate returned the SAME root node; the cache is " +
			"being handed out rather than cloned, so an in-place field rewrite corrupts every " +
			"later query over the concept")
	}
	firstLogical, ok := first.(*LogicalExpression)
	if !ok {
		t.Fatalf("composite predicate root is %T", first)
	}
	secondLogical := second.(*LogicalExpression)
	if firstLogical.Left == secondLogical.Left || firstLogical.Right == secondLogical.Right {
		t.Fatal("the composite predicate's clone is SHALLOW -- the branches are shared, so " +
			"rewriting a field on one query's injected term rewrites it on every other's")
	}

	// And the stamp, applied where the plan seam applies it.
	plan := &QueryPlan{
		Root:         &ComparisonExpression{Field: FieldReference{Raw: "status", Parts: []string{"status"}}, Operator: OpEq, Value: "running"},
		BoundConcept: declaredCompositeConcept,
	}
	if err := enforceRowAuthzOnPlan(plan); err != nil {
		t.Fatalf("enforceRowAuthzOnPlan: %v", err)
	}
	stamped := 0
	walkRowAuthzComparisons(plan.Root, func(c *ComparisonExpression) {
		if c.RowAuthzConcept == declaredCompositeConcept {
			stamped++
		}
	})
	if stamped == 0 {
		t.Fatalf("no comparison in the injected composite term carries RowAuthzConcept=%q. The "+
			"canonicalize-RHS pass then resolves the concept from extractConceptFromExpression, "+
			"which answers \"\" for this shape -- so the owner comparison tests a bare id against a "+
			"canonical stored one and matches nothing at all", declaredCompositeConcept)
	}
}

// walkRowAuthzComparisons visits every ComparisonExpression in a
// predicate tree. Test-local: the production walker is an implementation
// detail this test must not depend on the spelling of.
func walkRowAuthzComparisons(expr ExpressionNode, visit func(*ComparisonExpression)) {
	switch n := expr.(type) {
	case *ComparisonExpression:
		visit(n)
	case *LogicalExpression:
		walkRowAuthzComparisons(n.Left, visit)
		walkRowAuthzComparisons(n.Right, visit)
	}
}
