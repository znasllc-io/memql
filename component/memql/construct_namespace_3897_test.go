package memql

import (
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
)

// Namespacing the twelve flat construct kinds (memql#3897).
//
// `concept` has been namespaced since forever; the other twelve shared ONE flat
// registry keyed by the bare name, and the S5 uniqueness gate refused a
// duplicate at load. So a product DSL bundle could not declare a `shape`,
// `spec`, `query` or any other flat construct whose name a core construct
// already used -- a load-time error the product could not resolve except by
// renaming its own construct. Under platform consolidation a product IS a DSL
// bundle plus a client, so the primary delivery path was hitting a constraint
// with no way around it.

func shapeIn(origin, name string) *ShapeDefinition {
	return &ShapeDefinition{
		Name:     name,
		Origin:   origin,
		KindRow:  true,
		Template: map[string]any{"id": "row.id", "from": origin},
	}
}

// ---------------------------------------------------------------------------
// acceptance 1: two namespaces, one name, each resolving to its own
// ---------------------------------------------------------------------------

func TestTwoNamespacesMayDeclareTheSameFlatConstruct(t *testing.T) {
	reg := newShapeRegistry()

	if err := reg.add(shapeIn("alpha/shapes.memql", "widgetFull")); err != nil {
		t.Fatalf("alpha registration: %v", err)
	}
	// THE ASSERTION THE WHOLE ISSUE IS ABOUT. Before memql#3897 this returned
	// `shape "widgetFull" already registered` -- the load-time error a product
	// bundle could not resolve except by renaming its own construct.
	if err := reg.add(shapeIn("beta/shapes.memql", "widgetFull")); err != nil {
		t.Fatalf("a second namespace must be able to declare the same flat construct "+
			"name -- this refusal is the constraint memql#3897 exists to remove: %v", err)
	}

	alpha, err := reg.Registry.Get("alpha.widgetFull")
	if err != nil {
		t.Fatalf("alpha.widgetFull: %v", err)
	}
	beta, err := reg.Registry.Get("beta.widgetFull")
	if err != nil {
		t.Fatalf("beta.widgetFull: %v", err)
	}
	if alpha.Template["from"] != "alpha/shapes.memql" {
		t.Errorf("alpha.widgetFull resolved to %v", alpha.Template["from"])
	}
	if beta.Template["from"] != "beta/shapes.memql" {
		t.Errorf("beta.widgetFull resolved to %v", beta.Template["from"])
	}
}

// ---------------------------------------------------------------------------
// and a bare reference to an ambiguous name is refused, not guessed
// ---------------------------------------------------------------------------

func TestAnAmbiguousBareNameIsRefusedRatherThanGuessed(t *testing.T) {
	reg := newShapeRegistry()
	mustAdd(t, reg, shapeIn("alpha/shapes.memql", "widgetFull"))
	mustAdd(t, reg, shapeIn("beta/shapes.memql", "widgetFull"))

	_, err := reg.Registry.Get("widgetFull")
	if err == nil {
		t.Fatal("a bare name declared in two namespaces has NO correct answer. Returning " +
			"one of them is the silent capture memql#3802 fixed for concepts -- the wrong " +
			"construct bound, with OK=true.")
	}
	// The refusal has to be actionable, and distinguishable from "not found":
	// an author meeting it after adding a product bundle would otherwise read
	// it as the construct having vanished.
	for _, want := range []string{"alpha.widgetFull", "beta.widgetFull", "namespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the ambiguity refusal must name both candidates and how to "+
				"disambiguate; %q missing from: %v", want, err)
		}
	}
}

// A bare name that is UNAMBIGUOUS still resolves, and that is the floor the
// whole migration stands on.
//
// A reference inside a compiled body is looked up at EXECUTION time, from a
// context with no namespace -- `newFunctionValidator(fns.LookupIndex(), nil)`
// is built per query and carries only auth.CallOrigin. If a bare lookup stopped
// resolving, every such reference in the tree would break at once. Instead the
// failure mode of an unreached reference is exactly the behaviour this tree
// already had.
func TestAnUnambiguousBareNameStillResolves(t *testing.T) {
	reg := newShapeRegistry()
	mustAdd(t, reg, shapeIn("alpha/shapes.memql", "onlyInAlpha"))

	got, err := reg.Registry.Get("onlyInAlpha")
	if err != nil {
		t.Fatalf("an unambiguous bare name must still resolve, or every reference in a "+
			"compiled body breaks at once: %v", err)
	}
	if got.Template["from"] != "alpha/shapes.memql" {
		t.Errorf("resolved to the wrong shape: %v", got.Template["from"])
	}
}

// An engine-internal construct with no origin keeps the bare key, and a
// namespaced alias must never evict it.
func TestAnUnnamespacedConstructIsNotEvictedByAnAlias(t *testing.T) {
	reg := newShapeRegistry()
	mustAdd(t, reg, shapeIn("", "internalOnly"))                   // no origin
	mustAdd(t, reg, shapeIn("alpha/shapes.memql", "internalOnly")) // same name, namespaced

	got, err := reg.Registry.Get("internalOnly")
	if err != nil {
		t.Fatalf("the real unnamespaced entry must win its own key: %v", err)
	}
	if got.Template["from"] != "" {
		t.Errorf("a namespaced alias evicted or shadowed the real unnamespaced entry: %v",
			got.Template["from"])
	}
	// And the namespaced one is still reachable by its own key.
	if _, err := reg.Registry.Get("alpha.internalOnly"); err != nil {
		t.Errorf("alpha.internalOnly: %v", err)
	}
}

// LookupIndex is what the execution-time consumers read. It must carry both
// spellings, and must omit the bare key exactly when it would be ambiguous.
func TestLookupIndexCarriesBothSpellingsAndOmitsAmbiguousOnes(t *testing.T) {
	reg := newShapeRegistry()
	mustAdd(t, reg, shapeIn("alpha/shapes.memql", "shared"))
	mustAdd(t, reg, shapeIn("beta/shapes.memql", "shared"))
	mustAdd(t, reg, shapeIn("alpha/shapes.memql", "unique"))

	idx := reg.Registry.LookupIndex()

	for _, key := range []string{"alpha.shared", "beta.shared", "alpha.unique", "unique"} {
		if idx[key] == nil {
			t.Errorf("LookupIndex is missing %q", key)
		}
	}
	if idx["shared"] != nil {
		t.Error("LookupIndex must NOT carry a bare key for an ambiguous name -- a consumer " +
			"doing idx[bare] would bind whichever namespace the map iteration reached")
	}
	// Counting must still come off Snapshot: this map is deliberately larger.
	if got := reg.Registry.Count(); got != 3 {
		t.Errorf("Count = %d, want 3 -- LookupIndex's aliases must not inflate the registry", got)
	}
}

// ---------------------------------------------------------------------------
// acceptance 2: resolution goes own-namespace first, then imports
// ---------------------------------------------------------------------------

func TestConstructScopeResolvesOwnNamespaceBeforeImports(t *testing.T) {
	known := map[string]bool{
		"cognition.widgetFull": true,
		"common.widgetFull":    true,
	}
	exists := func(key string) bool { return known[key] }

	scope := NewConstructScope("cognition/queries.memql", []*languageAst.UseDeclaration{
		{Parts: []string{"common", "shapes"}, Names: []string{"widgetFull"}},
	})

	got, ok := scope.Resolve("widgetFull", exists)
	if !ok || got != "cognition.widgetFull" {
		t.Errorf("a bare name must mean THIS namespace's construct even when a same-named "+
			"one is imported -- otherwise the import silently captures every bare use, "+
			"which is memql#3802's defect. got %q ok=%v", got, ok)
	}
}

func TestConstructScopeResolvesThroughAnImport(t *testing.T) {
	known := map[string]bool{"common.isActiveRecord": true}
	exists := func(key string) bool { return known[key] }

	scope := NewConstructScope("cognition/queries.memql", []*languageAst.UseDeclaration{
		{Parts: []string{"common", "traits"}, Names: []string{"isActiveRecord"}},
	})

	got, ok := scope.Resolve("isActiveRecord", exists)
	if !ok || got != "common.isActiveRecord" {
		t.Errorf("a cross-namespace reference must resolve through its import. got %q ok=%v", got, ok)
	}
}

// ALIASING FINALLY MEANS SOMETHING FOR THESE TWELVE KINDS. memql#3802 shipped
// `use x.y.{ n as m }` and it was INERT here: two same-named flat constructs
// could not coexist, so there was nothing to alias between. Namespacing is what
// makes it meaningful, which is the actual answer to "why can't I alias a shape".
func TestConstructScopeResolvesAnAliasedImport(t *testing.T) {
	known := map[string]bool{
		"cognition.widgetFull": true,
		"tools.widgetFull":     true,
	}
	exists := func(key string) bool { return known[key] }

	scope := NewConstructScope("cognition/queries.memql", []*languageAst.UseDeclaration{
		{
			Parts:   []string{"tools", "shapes"},
			Names:   []string{"widgetFull"},
			Aliases: map[string]string{"widgetFull": "toolsWidget"},
		},
	})

	// The ALIAS names the foreign one.
	if got, ok := scope.Resolve("toolsWidget", exists); !ok || got != "tools.widgetFull" {
		t.Errorf("the alias must name the foreign construct: got %q ok=%v", got, ok)
	}
	// And the bare name still means this namespace's, which is what makes
	// aliasing a fix rather than a second capture.
	if got, ok := scope.Resolve("widgetFull", exists); !ok || got != "cognition.widgetFull" {
		t.Errorf("a bare name alongside an aliased import must still mean this "+
			"namespace's: got %q ok=%v", got, ok)
	}
}

// A nested namespace is importable, and its `use` path spells the directory.
func TestConstructScopeHandlesANestedNamespace(t *testing.T) {
	known := map[string]bool{"agents/tools.askSpecialist": true}
	exists := func(key string) bool { return known[key] }

	scope := NewConstructScope("cognition/queries.memql", []*languageAst.UseDeclaration{
		{Parts: []string{"agents", "tools", "trainerTools"}, Names: []string{"askSpecialist"}},
	})

	got, ok := scope.Resolve("askSpecialist", exists)
	if !ok || got != "agents/tools.askSpecialist" {
		t.Errorf("a nested namespace must be importable, with the `use` path spelling the "+
			"directory (memql#3898). got %q ok=%v", got, ok)
	}
}

// ---------------------------------------------------------------------------
// key vocabulary
// ---------------------------------------------------------------------------

func TestConstructKeyRoundTrips(t *testing.T) {
	cases := []struct{ ns, name string }{
		{"cognition", "spaceParticipants"},
		{"agents/tools", "askSpecialist"}, // a nested namespace is a PATH
		{"", "engineInternal"},            // no origin: the bare key space
	}
	for _, tc := range cases {
		key := QualifyConstruct(tc.ns, tc.name)
		ns, name := SplitConstructKey(key)
		if ns != tc.ns || name != tc.name {
			t.Errorf("round trip of (%q, %q) via %q gave (%q, %q)", tc.ns, tc.name, key, ns, name)
		}
	}
}

func mustAdd(t *testing.T, reg *ShapeRegistry, shape *ShapeDefinition) {
	t.Helper()
	if err := reg.add(shape); err != nil {
		t.Fatalf("registering %s/%s: %v", shape.Origin, shape.Name, err)
	}
}
