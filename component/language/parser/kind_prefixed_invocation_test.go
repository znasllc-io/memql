package parser

import (
	"testing"
)

// Story 2 (#2324): kind-prefixed construct invocation + colon-named args +
// spec/trait predicate form. These are ADDITIVE and backward-compatible — the
// old forms (bare calls, `name({...})`, `spec("name")`, `key=value` args) must
// keep parsing unchanged; the old-form rejection lands later with Story 3
// (#2326).

// mustParseExpr parses a single expression or fails the test.
func mustParseExpr(t *testing.T, src string) ExpressionNode {
	t.Helper()
	expr, err := ParseExpression(src)
	if err != nil {
		t.Fatalf("ParseExpression(%q) error: %v", src, err)
	}
	if expr == nil {
		t.Fatalf("ParseExpression(%q) returned nil node", src)
	}
	return expr
}

func TestKindPrefixedCall_EachKindCarriesKind(t *testing.T) {
	cases := []struct {
		src      string
		wantKind string
		wantName string
	}{
		{"logic decideThing()", "logic", "decideThing"},
		{"query existingCluster()", "query", "existingCluster"},
		{"mutation createNode()", "mutation", "createNode"},
		{"action tagRelease()", "action", "tagRelease"},
		{"capability tagRelease()", "capability", "tagRelease"},
		{"builtin serviceVersion()", "builtin", "serviceVersion"},
		{"automation autoJoin()", "automation", "autoJoin"},
	}
	for _, tc := range cases {
		t.Run(tc.wantKind, func(t *testing.T) {
			expr := mustParseExpr(t, tc.src)
			fc, ok := expr.(*FunctionCallExpr)
			if !ok {
				t.Fatalf("expected *FunctionCallExpr, got %T", expr)
			}
			if fc.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", fc.Kind, tc.wantKind)
			}
			if fc.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", fc.Name, tc.wantName)
			}
			if fc.Args == nil {
				t.Errorf("Args is nil; empty () must yield a non-nil empty map")
			}
			if len(fc.Args) != 0 {
				t.Errorf("Args = %v, want empty", fc.Args)
			}
		})
	}
}

func TestKindPrefixedCall_AsAssignmentRHS(t *testing.T) {
	// `existing := query existingCluster()` — the kind-prefixed call must be a
	// valid right-hand-side expression. We parse the RHS directly.
	expr := mustParseExpr(t, "query existingCluster()")
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if fc.Kind != "query" || fc.Name != "existingCluster" {
		t.Fatalf("got Kind=%q Name=%q, want query/existingCluster", fc.Kind, fc.Name)
	}
}

func TestKindPrefixedCall_ColonNamedArgs(t *testing.T) {
	expr := mustParseExpr(t, "mutation createNode(id: args.node.id, nodeType: args.node.type)")
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if fc.Kind != "mutation" {
		t.Errorf("Kind = %q, want mutation", fc.Kind)
	}
	if _, ok := fc.Args["id"]; !ok {
		t.Errorf("missing colon-named arg %q; args=%v", "id", fc.Args)
	}
	if _, ok := fc.Args["nodeType"]; !ok {
		t.Errorf("missing colon-named arg %q; args=%v", "nodeType", fc.Args)
	}
	if len(fc.Args) != 2 {
		t.Errorf("Args len = %d, want 2 (%v)", len(fc.Args), fc.Args)
	}
	// The id value should resolve to the caller-arg reference, not a bare ident.
	if ref, ok := fc.Args["id"].(*ArgRefExpr); !ok || ref.Path != "node.id" {
		t.Errorf("Args[id] = %#v, want *ArgRefExpr{Path:node.id}", fc.Args["id"])
	}
}

func TestColonNamedArgs_OnBareCall(t *testing.T) {
	// Colon-named args apply uniformly to bare (unprefixed) calls too.
	expr := mustParseExpr(t, "createNode(id: args.node.id)")
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if fc.Kind != "" {
		t.Errorf("Kind = %q, want empty for a bare call", fc.Kind)
	}
	if _, ok := fc.Args["id"]; !ok {
		t.Errorf("missing colon-named arg %q; args=%v", "id", fc.Args)
	}
}

func TestKindPrefixedCall_NestedObjectValueArg(t *testing.T) {
	expr := mustParseExpr(t, `mutation emit(payload: { kind: "x", count: 3 })`)
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if fc.Kind != "mutation" {
		t.Errorf("Kind = %q, want mutation", fc.Kind)
	}
	val, ok := fc.Args["payload"]
	if !ok {
		t.Fatalf("missing payload arg; args=%v", fc.Args)
	}
	obj, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want map[string]any (nested object value)", val)
	}
	if obj["kind"] != "x" {
		t.Errorf("payload.kind = %v, want x", obj["kind"])
	}
}

func TestSpecPredicateForm(t *testing.T) {
	expr := mustParseExpr(t, "spec requiresOwner")
	ref, ok := expr.(*SpecReferenceExpr)
	if !ok {
		t.Fatalf("expected *SpecReferenceExpr, got %T", expr)
	}
	if ref.Name != "requiresOwner" {
		t.Errorf("Name = %q, want requiresOwner", ref.Name)
	}
	if ref.Trait {
		t.Errorf("Trait = true, want false for spec form")
	}
}

func TestTraitPredicateForm(t *testing.T) {
	expr := mustParseExpr(t, "trait isActiveRecord")
	ref, ok := expr.(*SpecReferenceExpr)
	if !ok {
		t.Fatalf("expected *SpecReferenceExpr, got %T", expr)
	}
	if ref.Name != "isActiveRecord" {
		t.Errorf("Name = %q, want isActiveRecord", ref.Name)
	}
	if !ref.Trait {
		t.Errorf("Trait = false, want true for trait form")
	}
}

func TestSpecPredicateForm_InFilterConjunction(t *testing.T) {
	// `filter active && spec requiresOwner` — predicate ref inside a boolean
	// expression. Parse just the boolean expression here.
	expr := mustParseExpr(t, "active && spec requiresOwner")
	le, ok := expr.(*LogicalExpr)
	if !ok {
		t.Fatalf("expected *LogicalExpr, got %T", expr)
	}
	ref, ok := le.Right.(*SpecReferenceExpr)
	if !ok {
		t.Fatalf("expected RHS *SpecReferenceExpr, got %T", le.Right)
	}
	if ref.Name != "requiresOwner" || ref.Trait {
		t.Errorf("got Name=%q Trait=%v, want requiresOwner/false", ref.Name, ref.Trait)
	}
}

// ---------------------------------------------------------------------------
// Regression: old forms must keep parsing UNCHANGED (rejection deferred to
// Story 3 / #2326).
// ---------------------------------------------------------------------------

func TestRegression_BareCoalesceStaysBare(t *testing.T) {
	expr := mustParseExpr(t, "coalesce(a, b)")
	if _, ok := expr.(*CoalesceExpr); !ok {
		t.Fatalf("coalesce(a,b) parsed as %T, want *CoalesceExpr (must stay a bare primitive)", expr)
	}
}

func TestRegression_BareFunctionCallNoKind(t *testing.T) {
	expr := mustParseExpr(t, "createNode(id: args.id)")
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if fc.Kind != "" {
		t.Errorf("bare call carried Kind = %q, want empty", fc.Kind)
	}
}

func TestRegression_CollectionMethodCallUndisturbed(t *testing.T) {
	expr := mustParseExpr(t, "args.members.where(m => m.active).count()")
	// The outer node should be a MethodCallExpr chain (count), not a generic
	// kind-tagged call.
	if _, ok := expr.(*MethodCallExpr); !ok {
		t.Fatalf("collection chain parsed as %T, want *MethodCallExpr", expr)
	}
}

func TestRegression_LegacyObjectLiteralPositionalArg(t *testing.T) {
	// `name({...})` — the legacy object-literal positional form must still
	// parse (drops into Args["0"] as the object value).
	expr := mustParseExpr(t, `createNode({ id: "x" })`)
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if len(fc.Args) != 1 {
		t.Fatalf("Args = %v, want one positional object arg", fc.Args)
	}
	obj, ok := fc.Args["0"].(map[string]any)
	if !ok {
		t.Fatalf("Args[0] = %T, want map[string]any", fc.Args["0"])
	}
	if obj["id"] != "x" {
		t.Errorf("Args[0].id = %v, want x", obj["id"])
	}
}

func TestRegression_LegacySpecStringCall(t *testing.T) {
	// `spec("name")` — legacy stringly call form still parses as a function
	// call (rejection deferred to Story 3). It must NOT be mistaken for the
	// `spec <name>` predicate form.
	expr := mustParseExpr(t, `spec("requiresOwner")`)
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr for spec(\"...\"), got %T", expr)
	}
	if fc.Name != "spec" {
		t.Errorf("Name = %q, want spec", fc.Name)
	}
	if fc.Kind != "" {
		t.Errorf("Kind = %q, want empty (legacy call form)", fc.Kind)
	}
	if got := fc.Args["0"]; got != "requiresOwner" {
		t.Errorf("Args[0] = %v, want requiresOwner", got)
	}
}

func TestRegression_LegacyEqualsNamedArgs(t *testing.T) {
	// `key=value` named args must keep working alongside the new colon form.
	expr := mustParseExpr(t, `searchUsers(active=true, limit=10)`)
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", expr)
	}
	if _, ok := fc.Args["active"]; !ok {
		t.Errorf("missing =-named arg active; args=%v", fc.Args)
	}
	if _, ok := fc.Args["limit"]; !ok {
		t.Errorf("missing =-named arg limit; args=%v", fc.Args)
	}
}
