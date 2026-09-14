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

// TestShadowCreditsOnlySpellingsEvalExprDecidesAsTheGate pins the strictness
// of the plan-constant arm TestShadowReadsTheLoweredClusterOwnerGate proves
// through the lowering. A spelling is credited as the cluster-owner gate only
// when EvalExpr decides it as the gate for every caller: the optional hop and
// parentheses read the same value (the envelope is always bound), while a
// string literal compares a bool with a string, a misspelled key reads an
// absent one, and an inverted or negated gate admits the wrong callers --
// crediting any of those would report a read as restating the tier while it
// does something else. Built by hand rather than lowered: whether Lower
// admits each spelling is Lower's business.
func TestShadowCreditsOnlySpellingsEvalExprDecidesAsTheGate(t *testing.T) {
	composite := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true}
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
		`actor.isClusterOwner != true`,
		`!actor.isClusterOwner`,
	} {
		if got, _ := AnalyzeShadow(planGate(gate), composite); got == ShadowAlreadyImplied {
			t.Errorf("`%s` was credited as the cluster-owner gate; EvalExpr never decides it as one, so the disjunction does not restate the tier", gate)
		}
	}
}
