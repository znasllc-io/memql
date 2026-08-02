package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lane1_pinned_namespace_test.go -- memql#2945.
//
// memqllint and boot disagreed about what a `use` path IS.
//
//	boot    a NAMESPACE hint, matched as ":"+hint+":" against canonical ids
//	        (concept_resolver.go: namespaceHintForName ->
//	        resolveBareConceptNameWithNamespace). It never reads a directory.
//	lane 1  a FILE PATH -- <seg>/concepts.memql looked up in Tree.Files, then
//	        the symbol looked up in that one file (resolveUseModule).
//
// For an UNPINNED domain the directory name and the namespace are the same
// string, so the two models coincide and the divergence is invisible. That is
// why every fixture in this package predating #2945 is unpinned or flat, and
// why this survived.
//
// Under a namespace.pin they are different strings, and the lint rejected an
// import the engine binds. The ruling on #2945 was that lane 1 is wrong: a
// lint exists to predict boot, so where they differ the lint is wrong by
// definition. These tests pin the agreement for the PINNED case specifically.
//
// TWO of the tests here fail against the file-path model and pass against the
// namespace model: TestLane1_PinnedNamespaceImportIsAccepted, and
// TestLane1_PinnedNamespaceStillRejectsAGenuinelyMissingSymbol on its message
// assertions (its primary check passes either way).
//
// The other two do NOT, and the header used to claim otherwise, which
// overstates the evidence for the next reader.
// TestLane1_UnpinnedTreeVerdictIsUnchanged passes against both models BY
// DESIGN -- it is the regression floor. And
// TestLane1_PinnedNamespaceImportSilencesTheAmbiguity also passes against the
// file-path model, because lane 2 went inconclusive there rather than
// erroring; it is a forward guard, not a reproduction, so it is not evidence
// that the fix works.
//
// The other half of the agreement -- that a pin PERMITS a namespace rather
// than APPLYING one, so directory membership alone is the wrong test -- lives
// in lane1_pin_namespace_agreement_test.go.

// pinnedNamespaceWithRealDirTree is the shape the file-path model gets wrong.
//
// deploy/ is pinned to cluster, so its `widget` assembles to v1:cluster:widget.
// cluster/ EXISTS as a directory and declares only `gadget`. other/ declares a
// second `widget` so the bare name is genuinely ambiguous and the import is
// load-bearing rather than decorative.
//
// `use cluster.concepts.{ widget }` in deploy/queries.memql:
//
//	boot    nsHint "cluster" matches ":cluster:" against v1:cluster:widget,
//	        uniquely. Binds.
//	lane 1  cluster/concepts.memql exists and declares no widget. Rejects
//	        with `"widget" is not declared in cluster/concepts.memql`.
//
// The existence of cluster/ is the whole point. Without it lane 1 short-circuits
// on `!idx.namespaces[parts[0]]` and treats the path as an external namespace,
// so the identical import lints CLEAN -- the accepted / rejected / ignored
// three-way split #2945 describes.
func pinnedNamespaceWithRealDirTree() fstest.MapFS {
	return fstest.MapFS{
		"deploy/namespace.pin": file("cluster\n"),
		"deploy/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Declared under deploy/, namespaced to cluster.")
concept widget {
  label  string  @required @description("Label.")
}`),
		// The pin's directory exists and declares something ELSE. This is what
		// makes lane 1 reject rather than skip.
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("The pin target exists but declares no widget.")
concept gadget {
  label  string  @required @description("Label.")
}`),
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
@description("A second widget, so the bare name is ambiguous without the import.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"deploy/queries.memql": file(`use cluster.concepts.{ widget }

@enabled
@description("Binds widget by its PINNED namespace.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}
}

// TestLane1_PinnedNamespaceImportIsAccepted is the core #2945 assertion: the
// spelling boot binds must not be reported by the lint.
func TestLane1_PinnedNamespaceImportIsAccepted(t *testing.T) {
	tree := loadTree(t, pinnedNamespaceWithRealDirTree())

	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "is not declared in") {
			t.Errorf("lane 1 rejected `use cluster.concepts.{ widget }`, which boot binds.\n"+
				"deploy/ is pinned to cluster, so its widget assembles to v1:cluster:widget and "+
				"boot's nsHint \"cluster\" matches \":cluster:\" uniquely. Lane 1 looked only in "+
				"cluster/, which declares gadget, and called the symbol missing -- the file-path "+
				"model of a use path. A lint exists to predict boot; where they differ the lint "+
				"is wrong (memql#2945).\n  got: %s", e)
		}
	}
}

// TestLane1_PinnedNamespaceImportSilencesTheAmbiguity is the other half, and
// it is what makes the acceptance above worth anything.
//
// The import must not merely be tolerated -- it must actually DO the job it
// exists for. `widget` is declared twice, so without a working import lane 2
// reports the binding as ambiguous (boot genuinely cannot resolve it). If the
// import were accepted but not consulted, this fixture would lint clean on
// lane 1 and still fail lane 2, leaving the author with no legal spelling --
// memql#2805's unsatisfiable-lint shape.
func TestLane1_PinnedNamespaceImportSilencesTheAmbiguity(t *testing.T) {
	tree := loadTree(t, pinnedNamespaceWithRealDirTree())

	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "ambiguous") || strings.Contains(e.Error(), "cannot disambiguate") {
			t.Errorf("lane 2 still reported an ambiguity for a name the import disambiguates. "+
				"Accepting the import without consulting it leaves NO spelling that passes both "+
				"lanes (memql#2805, memql#2945).\n  got: %s", e)
		}
	}
}

// TestLane1_PinnedNamespaceStillRejectsAGenuinelyMissingSymbol is the guard
// against over-widening.
//
// #2945 is satisfied by deleting lane 1, and that would be a strictly worse
// bug: memqllint is the only pre-boot gate a product bundle has. The namespace
// model must still prove a symbol absent when it is absent from EVERY domain
// belonging to the namespace -- here cluster/ and deploy/ both.
func TestLane1_PinnedNamespaceStillRejectsAGenuinelyMissingSymbol(t *testing.T) {
	root := pinnedNamespaceWithRealDirTree()
	root["deploy/queries.memql"] = file(`use cluster.concepts.{ widget, sprocket }

@enabled
@description("Imports one real name and one that exists nowhere.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`)
	tree := loadTree(t, root)

	var got string
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "sprocket") {
			got = e.Error()
		}
	}
	if got == "" {
		t.Fatal("lane 1 did not report `sprocket`, which is declared in NO directory belonging to " +
			"the cluster namespace. Widening the module lookup to a namespace must not stop it " +
			"proving a symbol absent -- memqllint is the only pre-boot gate a bundle has (memql#2945).")
	}
	// The message must name every file that was actually searched. Naming only
	// cluster/concepts.memql would send the author to add the concept to the
	// wrong domain.
	for _, want := range []string{"cluster/concepts.memql", "deploy/concepts.memql"} {
		if !strings.Contains(got, want) {
			t.Errorf("the missing-symbol message does not name %q, one of the files searched. "+
				"An author told a symbol is missing from a namespace needs to know which files "+
				"that namespace covers.\n  got: %s", want, got)
		}
	}
	// And it must not have swallowed the good name along with the bad one.
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), `"widget" is not declared`) {
			t.Errorf("lane 1 reported `widget` too, which IS declared in deploy/concepts.memql "+
				"under the cluster namespace.\n  got: %s", e)
		}
	}
}

// TestLane1_UnpinnedTreeVerdictIsUnchanged is the regression floor.
//
// The namespace model is a pure widening: domainsForNamespace always yields
// the literal directory first, and appends only domains that pinned themselves
// to it. On a tree where nothing is pinned the set is one element long and
// every verdict is byte-for-byte what the file-path model gave.
//
// This is what makes the change safe to land: it cannot alter the engine tree,
// or any product bundle that does not use namespace.pin.
func TestLane1_UnpinnedTreeVerdictIsUnchanged(t *testing.T) {
	// Same shape as the pinned fixture, minus the pin. `widget` now assembles
	// to v1:deploy:widget, so the cluster import is genuinely wrong and lane 1
	// must still say so.
	root := pinnedNamespaceWithRealDirTree()
	delete(root, "deploy/namespace.pin")
	root["deploy/concepts.memql"] = file(`@version("1.0.0")
@namespace("deploy")
@description("Unpinned: the id follows the directory.")
concept widget {
  label  string  @required @description("Label.")
}`)
	tree := loadTree(t, root)

	var found bool
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), `"widget" is not declared in cluster/concepts.memql`) {
			found = true
		}
	}
	if !found {
		t.Error("without a pin, `use cluster.concepts.{ widget }` names a namespace deploy/ does " +
			"NOT assemble under, so boot's \":cluster:\" cannot match it and lane 1 must still " +
			"reject. The widening fired on a tree with no namespace.pin at all, which is exactly " +
			"the blast radius memql#2945 promised not to have.")
	}
}
