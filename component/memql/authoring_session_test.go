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
