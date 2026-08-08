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

func TestEnsureBoundConceptFilter_DescendsIntoShapeWrapper(t *testing.T) {
	// Mirrors the AST shape of `magicLinkRequestByTokenHash`:
	//   shape("...") wrapping a filter Comparison.
	innerFilter := &ComparisonExpression{
		Field:    FieldReference{Raw: "payload.tokenHash", Parts: []string{"payload", "tokenHash"}},
		Operator: OpEq,
		Value:    "deadbeef",
	}
	shape := &ShapeExpression{
		Target:       innerFilter,
		TemplateName: "magicLinkRequestFull",
	}
	got := ensureBoundConceptFilter(shape, "v1:identity:magicLinkRequest")
	// Shape must remain the outermost node (parser enforces it).
	outShape, ok := got.(*ShapeExpression)
	if !ok {
		t.Fatalf("expected ShapeExpression at the outermost, got %T", got)
	}
	if outShape != shape {
		t.Fatalf("expected the same ShapeExpression instance to be returned")
	}
	// The AND should now live INSIDE the shape, around the filter.
	logic, ok := outShape.Target.(*LogicalExpression)
	if !ok {
		t.Fatalf("expected shape.Target to be LogicalExpression, got %T", outShape.Target)
	}
	if logic.Op != LogicalAnd {
		t.Fatalf("expected LogicalAnd inside shape, got %v", logic.Op)
	}
	if logic.Left != innerFilter {
		t.Fatalf("expected the original filter to survive on the AND's left side")
	}
	right, _ := logic.Right.(*ComparisonExpression)
	if right == nil || right.Field.Raw != "concept" || right.Value != "v1:identity:magicLinkRequest" {
		t.Fatalf("expected concept==boundConcept on the AND's right side, got %+v", logic.Right)
	}
}

func TestEnsureBoundConceptFilter_DescendsThroughChainedDirectives(t *testing.T) {
	// shape(paginate(sort(filter, ...))) -- chain of directives wrapping
	// the actual filter expression. The AND must land on the filter,
	// keeping every directive layer outermost.
	innerFilter := &ComparisonExpression{
		Field:    FieldReference{Raw: "payload.active", Parts: []string{"payload", "active"}},
		Operator: OpEq,
		Value:    true,
	}
	sortNode := &SortExpression{Target: innerFilter}
	paginateNode := &PaginateExpression{Target: sortNode}
	shape := &ShapeExpression{Target: paginateNode}

	got := ensureBoundConceptFilter(shape, "v1:identity:magicLinkRequest")

	outShape, ok := got.(*ShapeExpression)
	if !ok {
		t.Fatalf("outermost: expected ShapeExpression, got %T", got)
	}
	outPaginate, ok := outShape.Target.(*PaginateExpression)
	if !ok {
		t.Fatalf("layer 2: expected PaginateExpression, got %T", outShape.Target)
	}
	outSort, ok := outPaginate.Target.(*SortExpression)
	if !ok {
		t.Fatalf("layer 3: expected SortExpression, got %T", outPaginate.Target)
	}
	logic, ok := outSort.Target.(*LogicalExpression)
	if !ok {
		t.Fatalf("inner: expected LogicalExpression, got %T", outSort.Target)
	}
	if logic.Left != innerFilter {
		t.Fatalf("inner filter not preserved on AND's left")
	}
}

func TestEnsureBoundConceptFilter_ShapeWrappingNilTargetGetsConceptOnly(t *testing.T) {
	// `query foo { shape someShape }` with no filter -- Target is nil.
	// The wrap should produce shape(concept==boundConcept).
	shape := &ShapeExpression{Target: nil}
	got := ensureBoundConceptFilter(shape, "v1:identity:clusterSettings")
	outShape, ok := got.(*ShapeExpression)
	if !ok {
		t.Fatalf("outermost: expected ShapeExpression, got %T", got)
	}
	cmp, ok := outShape.Target.(*ComparisonExpression)
	if !ok || cmp.Field.Raw != "concept" || cmp.Value != "v1:identity:clusterSettings" {
		t.Fatalf("expected concept==boundConcept inside shape, got %+v", outShape.Target)
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
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and test/dslconformance/embed_test.go.")
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
