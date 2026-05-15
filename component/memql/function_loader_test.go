package memql

import "testing"

func TestNormalizeBuiltinArgContract_DefaultsToNone(t *testing.T) {
	contract, err := normalizeBuiltinArgContract(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contract == nil || contract.Profile != BuiltinArgProfileNone {
		t.Fatalf("expected default profile none, got %#v", contract)
	}
}

func TestNormalizeBuiltinArgContract_RequiresStringKeyForStringProfiles(t *testing.T) {
	_, err := normalizeBuiltinArgContract(&builtinArgContractDefinition{
		Profile: string(BuiltinArgProfileStringOrObject),
	})
	if err == nil {
		t.Fatalf("expected error for missing stringKey")
	}
}

func TestEnsureBoundConceptFilter_NilExpressionReturnsBareComparison(t *testing.T) {
	got := ensureBoundConceptFilter(nil, "v1:identity:magicLinkRequest")
	cmp, ok := got.(*ComparisonExpression)
	if !ok {
		t.Fatalf("expected *ComparisonExpression, got %T", got)
	}
	if cmp.Field.Raw != "concept" || cmp.Operator != OpEq {
		t.Fatalf("unexpected comparison shape: %+v", cmp)
	}
	if v, _ := cmp.Value.(string); v != "v1:identity:magicLinkRequest" {
		t.Fatalf("unexpected value: %v", cmp.Value)
	}
}

func TestEnsureBoundConceptFilter_WrapsExistingFilterInAnd(t *testing.T) {
	base := &ComparisonExpression{
		Field:    FieldReference{Raw: "payload.tokenHash", Parts: []string{"payload", "tokenHash"}},
		Operator: OpEq,
		Value:    "deadbeef",
	}
	got := ensureBoundConceptFilter(base, "v1:identity:magicLinkRequest")
	logic, ok := got.(*LogicalExpression)
	if !ok {
		t.Fatalf("expected *LogicalExpression, got %T", got)
	}
	if logic.Op != LogicalAnd {
		t.Fatalf("expected LogicalAnd, got %v", logic.Op)
	}
	if logic.Left != base {
		t.Fatalf("left side should preserve original expression by identity")
	}
	right, ok := logic.Right.(*ComparisonExpression)
	if !ok || right.Field.Raw != "concept" || right.Value != "v1:identity:magicLinkRequest" {
		t.Fatalf("right side should be concept==boundConcept, got %+v", logic.Right)
	}
}

func TestEnsureBoundConceptFilter_NoopWhenAlreadyPresent(t *testing.T) {
	existing := &LogicalExpression{
		Op: LogicalAnd,
		Left: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.tokenHash", Parts: []string{"payload", "tokenHash"}},
			Operator: OpEq,
			Value:    "deadbeef",
		},
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:identity:magicLinkRequest",
		},
	}
	got := ensureBoundConceptFilter(existing, "v1:identity:magicLinkRequest")
	if got != existing {
		t.Fatalf("expected the same expression node (no rewrap); got %T (%p) want %p", got, got, existing)
	}
}

func TestEnsureBoundConceptFilter_AddsWhenConceptEqualityIsForDifferentId(t *testing.T) {
	existing := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    "v1:identity:user",
	}
	got := ensureBoundConceptFilter(existing, "v1:identity:magicLinkRequest")
	logic, ok := got.(*LogicalExpression)
	if !ok {
		t.Fatalf("expected *LogicalExpression, got %T", got)
	}
	right, _ := logic.Right.(*ComparisonExpression)
	if right == nil || right.Value != "v1:identity:magicLinkRequest" {
		t.Fatalf("expected new AND clause binding magicLinkRequest, got %+v", logic.Right)
	}
}

func TestEnsureBoundConceptFilter_TreatsOrAsAbsent(t *testing.T) {
	// concept==X inside an OR branch can't constrain every path, so
	// the helper should still add its AND clause.
	orExpr := &LogicalExpression{
		Op: LogicalOr,
		Left: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:identity:magicLinkRequest",
		},
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.email", Parts: []string{"payload", "email"}},
			Operator: OpEq,
			Value:    "x@example.com",
		},
	}
	got := ensureBoundConceptFilter(orExpr, "v1:identity:magicLinkRequest")
	logic, ok := got.(*LogicalExpression)
	if !ok {
		t.Fatalf("expected LogicalExpression wrapper, got %T", got)
	}
	if logic.Op != LogicalAnd || logic.Left != orExpr {
		t.Fatalf("expected AND wrap preserving original OR on the left: %+v", logic)
	}
}

func TestLoadBuiltinFunctions_RegistersAliasesAndArgs(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	builtins, err := loadBuiltinFunctions(nil)
	if err != nil {
		t.Fatalf("loadBuiltinFunctions: %v", err)
	}
	serviceVersion, ok := builtins["serviceVersion"]
	if !ok || serviceVersion == nil {
		t.Fatalf("expected serviceVersion builtin to be loaded")
	}
	if len(serviceVersion.BuiltinAliases) == 0 || serviceVersion.BuiltinAliases[0] != "memqlVersion" {
		t.Fatalf("expected memqlVersion alias, got %#v", serviceVersion.BuiltinAliases)
	}
	if serviceVersion.BuiltinArgs == nil || serviceVersion.BuiltinArgs.Profile != BuiltinArgProfileNone {
		t.Fatalf("expected serviceVersion args profile none, got %#v", serviceVersion.BuiltinArgs)
	}
}
