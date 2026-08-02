package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// graph_pinned_namespace_test.go -- memql#2945, landing review.
//
// The editor half of #2945 shipped with NO coverage anywhere in the repo:
// reverting ModuleResolves / ModuleSymbols / SymbolDeclared to the old
// first-target-only behaviour -- undoing the fix on the Sense side entirely --
// left every test in the repo green. Index is what MemQL Sense consumes for
// completion and diagnostics, so an untested Index means the editor can
// contradict both the lint and boot with nothing failing.
//
// Its own doc comment says its purpose is that "the editor agrees with what
// memqllint would accept". These assert that.

// twoPinnedDomainsTree puts TWO directories in one namespace, which is the
// whole reason the resolver returns a slice -- and which no fixture in the
// package exercised. `cluster` has no directory of its own here: it exists
// only as a namespace that alpha/ and beta/ pin themselves into.
func twoPinnedDomainsTree() fstest.MapFS {
	return fstest.MapFS{
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("The literal directory, declaring its own concept.")
concept gadget {
  label  string  @required @description("Label.")
}`),
		"alpha/namespace.pin": file("cluster\n"),
		"alpha/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Pinned into cluster.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"beta/namespace.pin": file("cluster\n"),
		"beta/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Also pinned into cluster.")
concept sprocket {
  label  string  @required @description("Label.")
}`),
	}
}

// The editor must offer the union across every directory in the namespace --
// that is the completion list an author sees. A first-target-only Index offers
// only the literal directory's symbols and silently omits the rest.
func TestIndex_ModuleSymbolsUnionsThePinnedNamespace(t *testing.T) {
	ix := loadTree(t, twoPinnedDomainsTree()).NewIndex()

	names, importsOnly, ok := ix.ModuleSymbols("cluster", "concepts")
	if !ok {
		t.Fatal("cluster.concepts must resolve: a literal directory and two pinned domains")
	}
	if importsOnly {
		t.Fatal("no target here is imports-only")
	}

	for _, want := range []string{"gadget", "widget", "sprocket"} {
		var found bool
		for _, got := range names {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("completion omitted %q from namespace cluster (got %v). The editor must "+
				"offer every symbol the namespace supplies, or it contradicts the lint that "+
				"accepts importing it.", want, names)
		}
	}
}

// Sorted output, and identical across runs. pinnedTo is built by ranging a map,
// so without the sort the order -- and the "any of X, Y" diagnostic that renders
// from it -- is nondeterministic. Deleting the sort was previously undetectable
// because no fixture pinned two domains to one namespace.
func TestIndex_PinnedNamespaceOrderIsDeterministic(t *testing.T) {
	var first []string
	for i := 0; i < 25; i++ {
		ix := loadTree(t, twoPinnedDomainsTree()).NewIndex()
		names, _, ok := ix.ModuleSymbols("cluster", "concepts")
		if !ok {
			t.Fatal("cluster.concepts must resolve")
		}
		if i == 0 {
			first = names
			continue
		}
		if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("ModuleSymbols is nondeterministic across runs: %v vs %v", first, names)
		}
	}

	// pinnedTo is built by ranging a map, so a missing sort shows up as an
	// order that varies between builds rather than one that is wrong every
	// time. Rebuild repeatedly: one sample cannot distinguish "sorted" from
	// "happened to come out sorted".
	for i := 0; i < 50; i++ {
		idx := loadTree(t, twoPinnedDomainsTree()).buildDeclIndex()
		dirs := idx.domainsForNamespace("cluster")
		if strings.Join(dirs, ",") != "cluster,alpha,beta" {
			t.Fatalf("domainsForNamespace must yield the literal directory first, then the "+
				"pinned domains SORTED -- otherwise the \"any of X, Y\" diagnostic renders in a "+
				"different order run to run. Got %v on build %d.", dirs, i)
		}
	}
}

// SymbolDeclared is what Sense uses to decide whether to squiggle an import.
// It must agree with lane 1 on the same tree, in both directions.
func TestIndex_SymbolDeclaredAgreesWithLaneOne(t *testing.T) {
	ix := loadTree(t, twoPinnedDomainsTree()).NewIndex()

	for _, name := range []string{"gadget", "widget", "sprocket"} {
		declared, decidable := ix.SymbolDeclared("cluster", "concepts", name)
		if !decidable {
			t.Fatalf("%q: the editor must be able to decide this", name)
		}
		if !declared {
			t.Errorf("the editor reports %q undeclared in cluster, but lane 1 accepts importing "+
				"it -- the editor would squiggle a legal import", name)
		}
	}

	declared, decidable := ix.SymbolDeclared("cluster", "concepts", "nonesuch")
	if decidable && declared {
		t.Error("the editor reports a symbol declared nowhere as declared")
	}
}

// The editor must apply the same per-declaration namespace test as the lint.
// A pinned directory whose decl carries no @namespace assembles under the
// DIRECTORY, so the namespace does not supply it and the editor must not
// offer it -- otherwise completion suggests an import boot refuses.
func TestIndex_DoesNotOfferDeclsThePinDoesNotNamespace(t *testing.T) {
	ix := loadTree(t, pinnedDirWithUnannotatedDeclTree()).NewIndex()

	declared, decidable := ix.SymbolDeclared("cluster", "concepts", "widget")
	if decidable && declared {
		t.Error("the editor offers `use cluster.concepts.{ widget }` for a decl that assembles " +
			"v1:deploy:widget -- an import boot refuses. The editor must apply the same " +
			"per-declaration test as lane 1, or it teaches authors a spelling that fails at boot.")
	}
}
