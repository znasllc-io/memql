package memql

import (
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestShadowReadsTheIRNotTheEdition pins why the shadow analyzer needs no
// edition-2026 port of its own (epic memql#5363), and what holds until the
// v1 filter lowering reaches it.
//
// The analyzer reads the engine's filter IR, never source text, and in that
// IR `row.` stays the INTRINSIC namespace: rowIntrinsicFieldRef refuses
// `row.<payload field>`, so the lowering must hand the analyzer a v1
// `row.ownerUserId` as the payload reference it already understands -- which
// is why topLevelPayloadField keeps reading `["row","ownerUserId"]` as a row
// intrinsic (TestShadowWouldNarrow pins that case). A v1 filter that reaches
// the analyzer UNLOWERED arrives as a lambda, and must read as undecidable:
// never already-implied, which would understate the blast radius, and never
// would-narrow, which would overstate it.
func TestShadowReadsTheIRNotTheEdition(t *testing.T) {
	unlowered := and(fieldCmp("concept", OpEq, "v1:lab:widget"), &LambdaExpression{
		Params: []string{"row"},
		Body:   ownerScoped("row.ownerUserId"),
	})
	got, reason := AnalyzeShadow(unlowered, &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"})
	if got != ShadowUndecidable {
		t.Fatalf("an unlowered v1 filter read %q (%s); it must be undecidable until the lowering hands the analyzer IR", got, reason)
	}
	if reason == "" {
		t.Fatal("an undecidable verdict must say what it could not see")
	}
}

// TestShadowReadsTheLoweredClusterOwnerGate: the lowering has reached the
// analyzer, and it makes a condition with no row in it a plan constant -- so
// the composite tier's own spelling, `row.ownerUserId == actor.userId ||
// actor.isClusterOwner == true`, arrives with its cluster-owner arm as a
// PlanConstExpression, not as the `actor.` comparison the legacy converter
// built. Read as an opaque disjunction, every construct written that way went
// undecidable and the tree-wide gate (TestRowAuthzEnforcementLandGate) failed
// on the flip. Each spelling EvalExpr decides as the gate is credited, through
// the real lowering; a spelling that is never the gate -- a string literal, a
// misspelled key, the gate inverted, another actor field -- is not.
func TestShadowReadsTheLoweredClusterOwnerGate(t *testing.T) {
	composite := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true}
	lower := func(src string) ExpressionNode {
		t.Helper()
		lam, err := langparser.ParseV1Lambda(src)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		ir, err := lowerQueryFilter(lam, nil, nil, nil)
		if err != nil {
			t.Fatalf("lower %s: %v", src, err)
		}
		return ir
	}
	for _, src := range []string{
		`row => row.ownerUserId == actor.userId || actor.isClusterOwner == true`,
		`row => actor.isClusterOwner || row.ownerUserId == actor.userId`,
		`row => row.ownerUserId == actor.userId || actor.isClusterOwner != false`,
		`row => row.ownerUserId == actor.userId || true == actor.isClusterOwner`,
		`row => row.status == "open" && (row.ownerUserId == actor.userId || (actor.isClusterOwner == true))`,
	} {
		ir := lower(src)
		if !treeHasPlanConstant(ir) {
			t.Fatalf("%s lowered with no plan constant (%s): this test no longer reaches the arm it pins", src, canonicalExpression(ir))
		}
		if got, reason := AnalyzeShadow(ir, composite); got != ShadowAlreadyImplied {
			t.Errorf("%s lowers to %s, which reads %s (%s); it restates the composite tier, so it must read %s",
				src, canonicalExpression(ir), got, reason, ShadowAlreadyImplied)
		}
	}

	// Built by hand rather than lowered: whether Lower admits each of these
	// is Lower's business, and what is pinned here is how the analyzer reads
	// one if it arrives. The optional hop reads the same value as `.` (the
	// envelope is always bound); every spelling in the second list is never
	// the gate for any caller.
	planGate := func(gate string) ExpressionNode {
		t.Helper()
		node, err := langparser.ParseV1Expression(gate)
		if err != nil {
			t.Fatalf("parse %s: %v", gate, err)
		}
		return or(ownerScoped("ownerUserId"), &PlanConstExpression{Expr: node})
	}
	for _, gate := range []string{`actor.?isClusterOwner == true`, `(actor.isClusterOwner)`} {
		if got, reason := AnalyzeShadow(planGate(gate), composite); got != ShadowAlreadyImplied {
			t.Errorf("`%s` is the cluster-owner gate, but the disjunction reads %s (%s)", gate, got, reason)
		}
	}
	for _, gate := range []string{
		`actor.isClusterOwner == "true"`,
		`actor.isclusterowner == true`,
		`actor.isClusterOwner == false`,
		`actor.isClusterOwner != true`,
		`actor.role == "owner"`,
		`!actor.isClusterOwner`,
	} {
		if got, _ := AnalyzeShadow(planGate(gate), composite); got == ShadowAlreadyImplied {
			t.Errorf("`%s` was credited as the cluster-owner gate; EvalExpr never decides it as one, so the disjunction does not restate the tier", gate)
		}
	}
}
