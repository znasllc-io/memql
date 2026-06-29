package memql

import (
	"context"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// declToSpec is a test helper: parse a single struct-form spec/trait
// declaration and run it through the production converter.
func declToSpec(t *testing.T, src string) (*Spec, error) {
	t.Helper()
	decl, err := languageParser.ParseSpecDecl(src)
	if err != nil {
		t.Fatalf("ParseSpecDecl(%q) failed: %v", src, err)
	}
	return specDeclToSpec(decl, "test.memql")
}

// TestSpecRole_AllowsContextSpec confirms an actor-bound spec converts
// cleanly as a spec (not a trait). Under the epic #2281 binding model the
// spec binds its shape in the signature and reads the projected field by
// bare name. Classification (row vs context) is deferred to the
// engine-bootstrap binding resolver, so Kind is empty at conversion time.
func TestSpecRole_AllowsContextSpec(t *testing.T) {
	spec, err := declToSpec(t, `@description("owner only")
spec actorEnvelope requiresOwner {
  return role == "owner"
}`)
	if err != nil {
		t.Fatalf("context-spec should be a valid spec, got: %v", err)
	}
	if spec.IsTrait {
		t.Error("IsTrait = true, want false (this is a spec)")
	}
	if spec.BoundName != "actorEnvelope" {
		t.Errorf("BoundName = %q, want actorEnvelope", spec.BoundName)
	}
}

// TestSpecRole_AllowsTrait confirms a deliberately UNBOUND trait (a row
// predicate over bare payload fields) converts cleanly as a trait.
func TestSpecRole_AllowsTrait(t *testing.T) {
	spec, err := declToSpec(t, `@description("kind is assistant")
trait agentKindAssistant {
  return kind == "assistant"
}`)
	if err != nil {
		t.Fatalf("row predicate should be a valid trait, got: %v", err)
	}
	if !spec.IsTrait {
		t.Error("IsTrait = false, want true (this is a trait)")
	}
}

// TestRejectAuthzSpecInFilter_AllowsTrait confirms the retired filter-usage
// guard is a no-op: a data `trait` (IsTrait == true) bare-referenced in a
// filter passes. The trait-vs-spec ROLE split (#2034) is superseded by the
// binding model (epic #2281), so rejectAuthzSpecInFilter always returns nil.
func TestRejectAuthzSpecInFilter_AllowsTrait(t *testing.T) {
	reg := newSpecRegistry()
	if err := reg.add(&Spec{Name: "agentKindAssistant", Kind: SpecKindRow, IsTrait: true}); err != nil {
		t.Fatalf("seed trait: %v", err)
	}

	filter := &LogicalExpression{
		Op:   "&&",
		Left: &SpecReferenceExpression{Name: "agentKindAssistant"},
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.ownerUserId", Parts: []string{"payload", "ownerUserId"}},
			Operator: OpEq,
			Value:    "u1",
		},
	}
	if err := rejectAuthzSpecInFilter(filter, reg); err != nil {
		t.Fatalf("a trait in a filter must pass, got: %v", err)
	}
}

// TestEvaluateSpec_RejectsTraitAsAuthzGate locks the requires-slot
// half: a data `trait` (SpecKindRow) is not callable as an
// authorization gate via EvaluateSpec.
func TestEvaluateSpec_RejectsTraitAsAuthzGate(t *testing.T) {
	reg := newSpecRegistry()
	if err := reg.add(&Spec{
		Name:    "agentKindAssistant",
		Kind:    SpecKindRow,
		IsTrait: true,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.kind", Parts: []string{"payload", "kind"}},
			Operator: OpEq,
			Value:    "assistant",
		},
	}); err != nil {
		t.Fatalf("seed trait: %v", err)
	}
	e := &MemQLEngine{specs: reg}

	_, err := e.EvaluateSpec(context.Background(), "agentKindAssistant")
	if err == nil {
		t.Fatal("expected a trait (data predicate) to be rejected as an authorization gate")
	}
	if !strings.Contains(err.Error(), "trait") || !strings.Contains(err.Error(), "filter") {
		t.Errorf("error should name the trait + point at the filter slot, got: %v", err)
	}
}
