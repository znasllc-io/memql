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

// TestSpecRole_RejectsRowOnlySpec locks the declaration-time role
// split: a `spec` whose body reads only payload / row intrinsics is a
// data/state predicate and must be declared a `trait`.
func TestSpecRole_RejectsRowOnlySpec(t *testing.T) {
	_, err := declToSpec(t, `@description("kind is assistant")
spec specAgentKindAssistant {
  payload.kind == "assistant"
}`)
	if err == nil {
		t.Fatal("expected a row-only spec to be rejected (it must be declared a trait)")
	}
	if !strings.Contains(err.Error(), "declare it as a `trait`") {
		t.Errorf("error should point the author at `trait`, got: %v", err)
	}
}

// TestSpecRole_AllowsContextSpec confirms an authorization spec
// (reads actor.*) is accepted as a `spec`.
func TestSpecRole_AllowsContextSpec(t *testing.T) {
	spec, err := declToSpec(t, `@description("owner only")
spec requiresOwner {
  actor.role == "owner"
}`)
	if err != nil {
		t.Fatalf("context-spec should be a valid spec, got: %v", err)
	}
	if spec.IsTrait {
		t.Error("IsTrait = true, want false (this is a spec)")
	}
	if spec.Kind != SpecKindContext {
		t.Errorf("Kind = %q, want %q", spec.Kind, SpecKindContext)
	}
}

// TestSpecRole_AllowsReclassifiedTrait confirms the reclassified
// data predicate loads as a `trait`, classifies row, and so is usable
// in a query filter.
func TestSpecRole_AllowsReclassifiedTrait(t *testing.T) {
	spec, err := declToSpec(t, `@description("kind is assistant")
trait traitAgentKindAssistant {
  payload.kind == "assistant"
}`)
	if err != nil {
		t.Fatalf("reclassified payload predicate should be a valid trait, got: %v", err)
	}
	if !spec.IsTrait {
		t.Error("IsTrait = false, want true (this is a trait)")
	}
	if spec.Kind != SpecKindRow {
		t.Errorf("Kind = %q, want %q (payload body classifies row)", spec.Kind, SpecKindRow)
	}
}

// TestRejectAuthzSpecInFilter_RejectsSpec locks the filter-slot usage
// half of the split: a bare reference to an authorization `spec`
// (IsTrait == false) inside a query filter is rejected.
func TestRejectAuthzSpecInFilter_RejectsSpec(t *testing.T) {
	reg := newSpecRegistry()
	if err := reg.add(&Spec{Name: "requiresOwner", Kind: SpecKindContext, IsTrait: false}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	// Bare reference to the authz spec, composed into a filter.
	filter := &LogicalExpression{
		Op:   "&&",
		Left: &SpecReferenceExpression{Name: "requiresOwner"},
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.status", Parts: []string{"payload", "status"}},
			Operator: OpEq,
			Value:    "active",
		},
	}
	err := rejectAuthzSpecInFilter(filter, reg)
	if err == nil {
		t.Fatal("expected an authorization spec bare-referenced in a filter to be rejected")
	}
	if !strings.Contains(err.Error(), "requires") {
		t.Errorf("error should mention the `requires` slot, got: %v", err)
	}
}

// TestRejectAuthzSpecInFilter_AllowsTrait confirms a data `trait`
// (IsTrait == true) bare-referenced in a filter passes.
func TestRejectAuthzSpecInFilter_AllowsTrait(t *testing.T) {
	reg := newSpecRegistry()
	if err := reg.add(&Spec{Name: "traitAgentKindAssistant", Kind: SpecKindRow, IsTrait: true}); err != nil {
		t.Fatalf("seed trait: %v", err)
	}

	filter := &LogicalExpression{
		Op:   "&&",
		Left: &SpecReferenceExpression{Name: "traitAgentKindAssistant"},
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
		Name:    "traitAgentKindAssistant",
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

	_, err := e.EvaluateSpec(context.Background(), "traitAgentKindAssistant")
	if err == nil {
		t.Fatal("expected a trait (data predicate) to be rejected as an authorization gate")
	}
	if !strings.Contains(err.Error(), "trait") || !strings.Contains(err.Error(), "filter") {
		t.Errorf("error should name the trait + point at the filter slot, got: %v", err)
	}
}
