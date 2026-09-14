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
