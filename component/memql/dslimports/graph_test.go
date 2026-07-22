package dslimports

// Tests for the Index projection (graph.go): the module-layout + declared-symbol
// queries the MemQL Sense language service resolves imports and references
// against. These assert the resolution facts; the sense-side adapter
// (component/memql) maps them onto the inconclusive tri-state.

import (
	"reflect"
	"testing"
	"testing/fstest"
)

func graphSliceHas(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestIndex_ModuleAndSymbolResolution(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"fylo/concepts.memql": file(`@version("1.0.0")
@namespace("fylo")
concept order {
  id  string  @required
}

concept scanEvent {
  id  string  @required
}`),
		"fylo/queries.memql": file(`use fylo.concepts.{ order }

@enabled
query order listOrders {
  args { id  string  @required }
  filter  id == args.id
}`),
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
concept widget {
  id  string  @required
}`),
	})
	ix := tree.NewIndex()

	if got, want := ix.Namespaces(), []string{"fylo", "other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
	if got, want := ix.Kinds("fylo"), []string{"concepts", "queries"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds(fylo) = %v, want %v", got, want)
	}

	// ModuleResolves: right kind resolves, the wrong kind segment does not
	// (the user's symptom 1: `use fylo.concept.{...}`).
	if !ix.ModuleResolves("fylo", "concepts") {
		t.Error("ModuleResolves(fylo, concepts) = false, want true")
	}
	if ix.ModuleResolves("fylo", "concept") {
		t.Error("ModuleResolves(fylo, concept) = true, want false (wrong kind segment)")
	}

	// ModuleSymbols carries the declared ids, not a typo (symptom 2).
	names, importsOnly, ok := ix.ModuleSymbols("fylo", "concepts")
	if !ok || importsOnly {
		t.Fatalf("ModuleSymbols(fylo, concepts) ok=%v importsOnly=%v, want ok=true importsOnly=false", ok, importsOnly)
	}
	if !graphSliceHas(names, "order") || !graphSliceHas(names, "scanEvent") {
		t.Errorf("ModuleSymbols(fylo, concepts) = %v, want to contain order + scanEvent", names)
	}
	if graphSliceHas(names, "oder") {
		t.Errorf("ModuleSymbols(fylo, concepts) = %v, must not contain the typo oder", names)
	}

	if declared, decidable := ix.SymbolDeclared("fylo", "concepts", "order"); !declared || !decidable {
		t.Errorf("SymbolDeclared(fylo, concepts, order) = (%v,%v), want (true,true)", declared, decidable)
	}
	if declared, decidable := ix.SymbolDeclared("fylo", "concepts", "oder"); declared || !decidable {
		t.Errorf("SymbolDeclared(fylo, concepts, oder) = (%v,%v), want (false,true)", declared, decidable)
	}
	// A module that does not resolve is not decidable -- absence proves nothing.
	if declared, decidable := ix.SymbolDeclared("fylo", "concept", "order"); declared || decidable {
		t.Errorf("SymbolDeclared(fylo, concept, order) = (%v,%v), want (false,false)", declared, decidable)
	}
}

func TestIndex_ConceptDeclared(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"fylo/concepts.memql": file(`@version("1.0.0")
@namespace("fylo")
concept order {
  id  string  @required
}`),
		// gadget is declared in TWO namespaces -- an ambiguous bare name.
		"alpha/concepts.memql": file(`@version("1.0.0")
@namespace("alpha")
concept gadget {
  id  string  @required
}`),
		"beta/concepts.memql": file(`@version("1.0.0")
@namespace("beta")
concept gadget {
  id  string  @required
}`),
	})
	ix := tree.NewIndex()

	// A UNIQUE bare name resolves regardless of the hint -- the hint is a
	// tiebreaker, not a hard namespace filter (boot's resolveConceptByTrailing-
	// Segment). `order` exists only in fylo; a "beta" hint must NOT reject it.
	for _, hint := range []string{"", "fylo", "beta"} {
		if declared, decidable := ix.ConceptDeclared("order", hint); !declared || !decidable {
			t.Errorf("ConceptDeclared(order, %q) = (%v,%v), want (true,true) -- unique name resolves regardless of hint", hint, declared, decidable)
		}
	}

	// An ambiguous bare name (gadget in alpha AND beta): the hint disambiguates.
	if declared, decidable := ix.ConceptDeclared("gadget", "alpha"); !declared || !decidable {
		t.Errorf("ConceptDeclared(gadget, alpha) = (%v,%v), want (true,true)", declared, decidable)
	}
	if declared, decidable := ix.ConceptDeclared("gadget", ""); !declared || !decidable {
		t.Errorf(`ConceptDeclared(gadget, "") = (%v,%v), want (true,true) -- ambiguous name still exists`, declared, decidable)
	}
	if declared, decidable := ix.ConceptDeclared("gadget", "fylo"); declared || !decidable {
		t.Errorf("ConceptDeclared(gadget, fylo) = (%v,%v), want (false,true) -- ambiguous, hinted ns holds none", declared, decidable)
	}

	// The user's symptom 5: a signature concept that exists nowhere -- provably
	// absent from the tree with a workspace-local or empty hint.
	if declared, decidable := ix.ConceptDeclared("full", ""); declared || !decidable {
		t.Errorf(`ConceptDeclared(full, "") = (%v,%v), want (false,true)`, declared, decidable)
	}
	// Absent name + external hint -> inconclusive (it may live in that namespace).
	if declared, decidable := ix.ConceptDeclared("full", "platform"); declared || decidable {
		t.Errorf("ConceptDeclared(full, platform) = (%v,%v), want (false,false)", declared, decidable)
	}
}
