package memql

// Phase 3 (#1533) session-authoring tests: bundle splitting, the
// validate+register session path, the core-first execution overlay, and the
// owner-gated promotion into the durable registries. All pure -- no live DB.

import (
	"context"
	"testing"
)

const sessionConceptSrc = `@version("1.0.0")
@namespace("mcpsess")
@description("Session test widget")
concept mcpWidget {
  ownerUserId  string  @required
  label        string
}`

const sessionMutationSrc = `use mcpsess.concepts.{ mcpWidget }

@description("Create a session widget")
mutation mcpWidget mutationCreateMcpWidget {
  args {
    widgetId  string  @required
  }
  insert {
    id:    canonicalId(args.widgetId, mcpWidget)
    label: "x"
  }
}`

const sessionSpecSrc = `@description("session spec")
spec mcpSessSpec {
  actor.role == "admin"
}`

// SplitBundleSource recognizes concept + function-family + shape constructs and
// dedupes by (kind, name).
func TestSplitBundleSource(t *testing.T) {
	src := sessionConceptSrc + "\n\n" + sessionMutationSrc + "\n\n" + sessionSpecSrc
	got := SplitBundleSource(src)

	kinds := map[string]string{} // name -> kind
	for _, c := range got {
		kinds[c.Name] = c.Kind
	}
	if kinds["mcpWidget"] != "concept" {
		t.Errorf("mcpWidget kind = %q, want concept", kinds["mcpWidget"])
	}
	if kinds["mutationCreateMcpWidget"] != "mutation" {
		t.Errorf("mutationCreateMcpWidget kind = %q, want mutation", kinds["mutationCreateMcpWidget"])
	}
	if kinds["mcpSessSpec"] != "spec" {
		t.Errorf("mcpSessSpec kind = %q, want spec", kinds["mcpSessSpec"])
	}
}

// AuthorSessionBundle validates + registers a concept+mutation bundle into the
// owner-scoped registry, compiling the mutation to an executable *Function.
func TestAuthorSessionBundle_ValidatesAndRegisters(t *testing.T) {
	reg := NewAuthoredRuntimeRegistry()
	res, err := AuthorSessionBundle(reg, "owner-1", sessionConceptSrc+"\n\n"+sessionMutationSrc)
	if err != nil {
		t.Fatalf("author: %v (diagnostics %+v)", err, res.Diagnostics)
	}
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
	// The mutation is registered + compiled.
	c, ok := reg.Lookup("owner-1", "mutation", "mutationCreateMcpWidget")
	if !ok {
		t.Fatal("mutation not registered")
	}
	if fn, isFn := c.Compiled.(*Function); !isFn || fn == nil {
		t.Errorf("mutation Compiled = %T, want *Function", c.Compiled)
	}
}

// An invalid bundle registers nothing and reports the failure.
func TestAuthorSessionBundle_RejectsInvalid(t *testing.T) {
	reg := NewAuthoredRuntimeRegistry()
	res, err := AuthorSessionBundle(reg, "owner-1", `spec broken { actor.role == }`)
	if err == nil {
		t.Fatal("expected an error for an invalid bundle")
	}
	if res.OK {
		t.Error("result should be OK=false")
	}
	if reg.Count() != 0 {
		t.Errorf("nothing should be registered on failure, got %d", reg.Count())
	}
}

// Re-defining the same construct in a session bumps the version (the registry
// rejects a non-increasing version, so a flat re-register would fail).
func TestAuthorSessionBundle_RedefineBumpsVersion(t *testing.T) {
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionSpecSrc); err != nil {
		t.Fatalf("first author: %v", err)
	}
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionSpecSrc); err != nil {
		t.Fatalf("redefine should bump version, got: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "spec", "mcpSessSpec")
	if !ok || c.Version != 2 {
		t.Errorf("redefine version = %d (ok=%v), want 2", c.Version, ok)
	}
}

// buildAuthoredFunctionOverlay layers the owner's authored functions over core
// but NEVER shadows a core construct of the same name (core-first).
func TestBuildAuthoredFunctionOverlay_CoreFirst(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	coreFn := &Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "core"}
	if err := e.functions.Upsert(coreFn); err != nil {
		t.Fatalf("seed core: %v", err)
	}

	reg := NewAuthoredRuntimeRegistry()
	// An authored function that COLLIDES with core, plus one that is net-new.
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "sharedName", Version: 1, Status: AuthoredActive,
		Compiled: &Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "authored"}})
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "ownPrivate", Version: 1, Status: AuthoredActive,
		Compiled: &Function{Name: "ownPrivate", FunctionKind: "query", Enabled: true}})

	overlay := e.buildAuthoredFunctionOverlay("owner-1", reg)

	// Collision: core wins.
	if got, _ := overlay.Get("sharedName"); got == nil || got.ExprSource != "core" {
		t.Errorf("overlay sharedName should keep the CORE definition, got %+v", got)
	}
	// Net-new authored construct is present.
	if got, _ := overlay.Get("ownPrivate"); got == nil {
		t.Error("net-new authored function missing from overlay")
	}
}

// PromoteAuthoredConstruct registers a session function into the shared registry
// (callable by all), refuses to overwrite a core construct, and is re-promotable.
func TestPromoteAuthoredConstruct(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}

	// Net-new authored function -> promotes into the shared registry.
	netNew := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "promotedQuery", Status: AuthoredActive,
		Compiled: &Function{Name: "promotedQuery", FunctionKind: "query", Enabled: true}}
	if err := e.PromoteAuthoredConstruct(context.Background(), netNew); err != nil {
		t.Fatalf("promote net-new: %v", err)
	}
	if got, _ := e.functions.Get("promotedQuery"); got == nil {
		t.Error("promoted function not in the shared registry")
	}
	// Re-promotion replaces in place (no spurious core-shadow refusal).
	if err := e.PromoteAuthoredConstruct(context.Background(), netNew); err != nil {
		t.Errorf("re-promote should be allowed, got %v", err)
	}

	// A construct whose name a CORE function owns is refused.
	core := &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "core"}
	if err := e.functions.Upsert(core); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	shadow := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "coreOwned", Status: AuthoredActive,
		Compiled: &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "authored"}}
	if err := e.PromoteAuthoredConstruct(context.Background(), shadow); err == nil {
		t.Error("promotion must refuse to redefine a core construct")
	}
	// Core is untouched.
	if got, _ := e.functions.Get("coreOwned"); got == nil || got.ExprSource != "core" {
		t.Errorf("core construct was overwritten by a refused promotion: %+v", got)
	}
}

func mustRegister(t *testing.T, reg *AuthoredRuntimeRegistry, c *AuthoredConstruct) {
	t.Helper()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register %s/%s: %v", c.Kind, c.Name, err)
	}
}

// --- #1559: session-authored spec overlay (ExecuteAuthored path only) ---

// rowComparison builds a `payload.<field> == <value>` row-spec comparison.
func rowComparison(field string, value any) *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: "payload." + field, Parts: []string{"payload", field}},
		Operator: OpEq,
		Value:    value,
	}
}

// buildAuthoredSpecOverlay layers the owner's authored specs over the core specs
// but NEVER shadows a core spec of the same name (core-first), exactly like the
// function overlay. A net-new session spec is added; a colliding one is dropped.
func TestBuildAuthoredSpecOverlay_CoreFirst(t *testing.T) {
	coreSpecs := newSpecRegistry()
	coreActive := &Spec{Name: "specIsActiveRecord", Kind: SpecKindRow, Expr: rowComparison("active", true)}
	if err := coreSpecs.add(coreActive); err != nil {
		t.Fatalf("seed core spec: %v", err)
	}
	e := &MemQLEngine{specs: coreSpecs}

	reg := NewAuthoredRuntimeRegistry()
	// A session spec that COLLIDES with a core spec name, plus a net-new one.
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "specIsActiveRecord", Version: 1, Status: AuthoredActive,
		Compiled: &Spec{Name: "specIsActiveRecord", Kind: SpecKindRow, Expr: rowComparison("active", false)}})
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "mcpSessRowSpec", Version: 1, Status: AuthoredActive,
		Compiled: &Spec{Name: "mcpSessRowSpec", Kind: SpecKindRow, Expr: rowComparison("kind", "widget")}})

	overlay := e.buildAuthoredSpecOverlay("owner-1", reg)

	// Collision: core wins (the session redefinition is dropped, not applied).
	got := overlay["specIsActiveRecord"]
	if got == nil {
		t.Fatal("core spec missing from overlay")
	}
	if cmp, ok := got.Expr.(*ComparisonExpression); !ok || cmp.Value != true {
		t.Errorf("overlay specIsActiveRecord must keep the CORE body (active==true), got %+v", got.Expr)
	}
	// Net-new session spec is present.
	if overlay["mcpSessRowSpec"] == nil {
		t.Error("net-new session spec missing from overlay")
	}

	// A different owner can NOT see owner-1's session spec.
	otherOverlay := e.buildAuthoredSpecOverlay("owner-2", reg)
	if otherOverlay["mcpSessRowSpec"] != nil {
		t.Error("owner-2 must not resolve owner-1's session spec")
	}
}

// resolveAuthoredSpecOverlay inlines a session spec reference, resolves a
// colliding name to the CORE body (never-shadow), and detects cycles.
func TestResolveAuthoredSpecOverlay_InlineAndNeverShadow(t *testing.T) {
	coreSpecs := newSpecRegistry()
	if err := coreSpecs.add(&Spec{Name: "shared", Kind: SpecKindRow, Expr: rowComparison("core", true)}); err != nil {
		t.Fatalf("seed core spec: %v", err)
	}
	e := &MemQLEngine{specs: coreSpecs}

	reg := NewAuthoredRuntimeRegistry()
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "mcpSessRowSpec", Version: 1, Status: AuthoredActive,
		Compiled: &Spec{Name: "mcpSessRowSpec", Kind: SpecKindRow, Expr: rowComparison("kind", "widget")}})
	// Session spec that collides with the core "shared" name -- must be dropped.
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "shared", Version: 1, Status: AuthoredActive,
		Compiled: &Spec{Name: "shared", Kind: SpecKindRow, Expr: rowComparison("session", true)}})

	overlay := e.buildAuthoredSpecOverlay("owner-1", reg)

	// (1) A reference to the session spec inlines its body.
	resolved, err := e.resolveAuthoredSpecOverlay(&SpecReferenceExpression{Name: "mcpSessRowSpec"}, overlay)
	if err != nil {
		t.Fatalf("resolve session spec: %v", err)
	}
	cmp, ok := resolved.(*ComparisonExpression)
	if !ok {
		t.Fatalf("expected inlined ComparisonExpression, got %T", resolved)
	}
	if cmp.Field.Raw != "payload.kind" || cmp.Value != "widget" {
		t.Errorf("session spec body not inlined correctly: %+v", cmp)
	}

	// (2) A reference to a name a core spec owns resolves to the CORE body.
	resolvedShared, err := e.resolveAuthoredSpecOverlay(&SpecReferenceExpression{Name: "shared"}, overlay)
	if err != nil {
		t.Fatalf("resolve shared: %v", err)
	}
	sharedCmp, ok := resolvedShared.(*ComparisonExpression)
	if !ok || sharedCmp.Field.Raw != "payload.core" || sharedCmp.Value != true {
		t.Errorf("colliding name must resolve to the CORE spec body, got %+v", resolvedShared)
	}

	// (3) An unknown spec is an error.
	if _, err := e.resolveAuthoredSpecOverlay(&SpecReferenceExpression{Name: "nope"}, overlay); err == nil {
		t.Error("expected an error for an unknown spec reference")
	}

	// (4) A circular spec reference is detected, not infinite-looped.
	cyclic := map[string]*Spec{
		"a": {Name: "a", Expr: &SpecReferenceExpression{Name: "b"}},
		"b": {Name: "b", Expr: &SpecReferenceExpression{Name: "a"}},
	}
	if _, err := e.resolveAuthoredSpecOverlay(&SpecReferenceExpression{Name: "a"}, cyclic); err == nil {
		t.Error("expected a circular-reference error")
	}
}

// End-to-end through parseWithFunctions: a session-authored query whose filter
// references a session-authored spec resolves on the ExecuteAuthored path (the
// spec is fully inlined into plan.Root), while the public Parse path -- which
// passes a nil overlay -- leaves the reference unresolved against e.specs.
func TestParseWithFunctions_SessionSpecInAuthoredQueryFilter(t *testing.T) {
	e := newParserTestEngine(t) // initialized=true, core functions, e.specs is the embedded core registry.

	// Session-authored query: filter = payload.ownerUserId == "u1" && <session spec>.
	authoredQuery := &Function{
		Name:         "querySessionWidgets",
		FunctionKind: "query",
		Enabled:      true,
		Expr: &LogicalExpression{
			Op:    LogicalAnd,
			Left:  rowComparison("ownerUserId", "u1"),
			Right: &SpecReferenceExpression{Name: "mcpSessRowSpec"},
		},
	}

	reg := NewAuthoredRuntimeRegistry()
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "querySessionWidgets", Version: 1, Status: AuthoredActive,
		Compiled: authoredQuery})
	mustRegister(t, reg, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "mcpSessRowSpec", Version: 1, Status: AuthoredActive,
		Compiled: &Spec{Name: "mcpSessRowSpec", Kind: SpecKindRow, Expr: rowComparison("kind", "widget")}})

	fnOverlay := e.buildAuthoredFunctionOverlay("owner-1", reg)
	specOverlay := e.buildAuthoredSpecOverlay("owner-1", reg)

	// Authored path: the session spec is inlined; no SpecReferenceExpression survives.
	plan, err := e.parseWithFunctions("querySessionWidgets()", fnOverlay, specOverlay, false)
	if err != nil {
		t.Fatalf("authored parse: %v", err)
	}
	if hasSpecReference(plan.Root) {
		t.Errorf("session spec was not inlined on the authored path: %#v", plan.Root)
	}
	if !hasComparison(plan.Root, "payload.kind", "widget") {
		t.Errorf("expected the session spec body (payload.kind==widget) inlined into the filter, got %#v", plan.Root)
	}

	// Public path: nil overlay -> the spec reference is left for runtime e.specs,
	// which does NOT carry the session spec, so the reference is unresolved.
	pubPlan, err := e.parseWithFunctions("querySessionWidgets()", fnOverlay, nil, false)
	if err != nil {
		t.Fatalf("public parse: %v", err)
	}
	if !hasSpecReference(pubPlan.Root) {
		t.Error("public path must leave the session spec UNresolved (it is overlay-only)")
	}
	if e.specs != nil {
		if _, lookupErr := e.specs.Get("mcpSessRowSpec"); lookupErr == nil {
			t.Error("session spec must NOT be visible in the engine's core spec registry")
		}
	}
}

// hasSpecReference reports whether any SpecReferenceExpression survives in expr.
func hasSpecReference(expr ExpressionNode) bool {
	switch node := expr.(type) {
	case *SpecReferenceExpression:
		return true
	case *LogicalExpression:
		return hasSpecReference(node.Left) || hasSpecReference(node.Right)
	case *RelationshipExpression:
		return hasSpecReference(node.Target)
	default:
		return false
	}
}

// hasComparison reports whether expr contains a `field == value` comparison.
func hasComparison(expr ExpressionNode, field string, value any) bool {
	switch node := expr.(type) {
	case *ComparisonExpression:
		return node.Field.Raw == field && node.Value == value
	case *LogicalExpression:
		return hasComparison(node.Left, field, value) || hasComparison(node.Right, field, value)
	case *RelationshipExpression:
		return hasComparison(node.Target, field, value)
	default:
		return false
	}
}
