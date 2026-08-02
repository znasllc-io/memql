package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lane1_pin_namespace_agreement_test.go -- memql#2945, landing review.
//
// The first cut of #2945 made directory membership the whole test: a directory
// carrying `namespace.pin: cluster` was enrolled into namespace `cluster`, and
// any symbol it declared satisfied `use cluster.…`. That is not what a pin
// does. `ast.AssembleConceptIdFromDeclInDir` only PERMITS an explicit
// `@namespace` equal to the pin; an un-annotated decl still takes the
// DIRECTORY as its namespace. So a pinned directory routinely holds decls
// under two different namespaces, and enrolling the directory wholesale made
// the lint accept a binding boot REFUSES.
//
// That is the direction the widening was documented as unable to take, and it
// is worse than the bug #2945 fixed: a green lint over a tree that fails at
// boot. These tests pin agreement in BOTH directions, per declaration.

// pinnedDirWithUnannotatedDeclTree: deploy/ is pinned to cluster but its
// widget carries NO @namespace, so it assembles v1:deploy:widget -- not
// v1:cluster:widget. `use cluster.concepts.{ widget }` must therefore be
// REJECTED, because boot's ":cluster:" needle cannot match v1:deploy:widget
// and boot reports the name as ambiguous against other/'s widget.
func pinnedDirWithUnannotatedDeclTree() fstest.MapFS {
	return fstest.MapFS{
		// The pin is present but does nothing for an un-annotated decl.
		"deploy/namespace.pin": file("cluster\n"),
		"deploy/concepts.memql": file(`@version("1.0.0")
@description("Pinned directory, but this decl carries no @namespace.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("The pin target exists but declares no widget.")
concept gadget {
  label  string  @required @description("Label.")
}`),
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
@description("A second widget, so the bare name is ambiguous at boot.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"deploy/queries.memql": file(`use cluster.concepts.{ widget }

@enabled
@description("Imports a namespace this decl does NOT assemble under.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}
}

// The regression guard. A pin file alone must not enrol a declaration into a
// namespace it does not assemble under.
func TestLane1_PinDoesNotEnrolUnannotatedDecls(t *testing.T) {
	tree := loadTree(t, pinnedDirWithUnannotatedDeclTree())

	var got string
	for _, err := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "use cluster.concepts") {
			got = err.Error()
		}
	}
	if got == "" {
		t.Fatal("lane 1 accepted `use cluster.concepts.{ widget }` for a decl that assembles " +
			"v1:deploy:widget. A namespace.pin PERMITS an explicit @namespace, it does not APPLY " +
			"one, so this decl is not in the cluster namespace and boot refuses the binding " +
			"(\":cluster:\" matches neither v1:deploy:widget nor v1:other:widget -- boot reports " +
			"the name ambiguous). A lint that accepts what boot refuses is worse than the bug " +
			"#2945 fixed: it is green CI over a tree that fails at boot.")
	}
}

// The counterpart, so the guard above cannot be satisfied by refusing
// everything: the SAME directory, same pin, with the annotation present, must
// still be accepted. This is the case #2945 was filed to fix.
func TestLane1_PinnedNamespaceStillAcceptedWithAnnotation(t *testing.T) {
	tree := loadTree(t, pinnedNamespaceWithRealDirTree())

	for _, err := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "use cluster.concepts") {
			t.Fatalf("the per-declaration namespace test must not undo #2945 -- this decl "+
				"carries @namespace(\"cluster\") and assembles v1:cluster:widget, which boot "+
				"binds: %v", err)
		}
	}
}

// A symbol declared only in a directory that is in the TREE but not in the
// NAMESPACE must still be rejected. The pre-existing over-widening guard
// probed a symbol declared nowhere at all, which stays red under any widening
// short of deleting lane 1 -- so it could not detect over-widening.
func TestLane1_NamespaceDoesNotReachAnUnrelatedDirectory(t *testing.T) {
	root := pinnedNamespaceWithRealDirTree()
	root["deploy/queries.memql"] = file(`use cluster.concepts.{ widget, sprocket }

@enabled
@description("sprocket is declared only in other/, which is not in the cluster namespace.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`)
	root["other/concepts.memql"] = file(`@version("1.0.0")
@namespace("other")
@description("Declares sprocket, in a namespace the import does not name.")
concept widget {
  name  string  @required @description("Name.")
}

@description("Only ever declared here.")
concept sprocket {
  name  string  @required @description("Name.")
}`)

	var got string
	for _, err := range loadTree(t, root).VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "sprocket") {
			got = err.Error()
		}
	}
	if got == "" {
		t.Fatal("`use cluster.concepts.{ sprocket }` was accepted, but sprocket is declared " +
			"only in other/ -- a directory in the tree and NOT in the cluster namespace. The " +
			"namespace must reach the directories that assemble under it, not the whole tree.")
	}
}

// A colon-scoped pin is a legal spelling: ValidateAssemblyInputs accepts a
// colon-separated namespace, the loader never constrains a pin's shape, and
// colon-scoped namespaces are live in the corpus (cognition:client:tool). Boot
// binds `use cluster.concepts.{ widget }` here, because its ":cluster:" needle
// is contained in v1:cluster:rollout:widget. Enrolling only on the whole pin
// string left this diverging exactly as #2945 described.
func TestLane1_ColonScopedPinAgreesWithBoot(t *testing.T) {
	root := pinnedNamespaceWithRealDirTree()
	root["deploy/namespace.pin"] = file("cluster:rollout\n")
	root["deploy/concepts.memql"] = file(`@version("1.0.0")
@namespace("cluster:rollout")
@description("Colon-scoped pin: assembles v1:cluster:rollout:widget.")
concept widget {
  label  string  @required @description("Label.")
}`)

	for _, err := range loadTree(t, root).VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "use cluster.concepts") {
			t.Fatalf("a colon-scoped pin diverges from boot: the id is "+
				"v1:cluster:rollout:widget, which CONTAINS \":cluster:\", so boot binds this "+
				"import -- but the lint rejected it: %v", err)
		}
	}

	// ...and under the full pin, which parses as a single leading segment.
	scoped := pinnedNamespaceWithRealDirTree()
	scoped["deploy/namespace.pin"] = file("cluster:rollout\n")
	scoped["deploy/concepts.memql"] = root["deploy/concepts.memql"]
	scoped["deploy/queries.memql"] = file(`use cluster:rollout.concepts.{ widget }

@enabled
@description("Imports by the full pinned namespace.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`)
	for _, err := range loadTree(t, scoped).VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "use cluster:rollout.concepts") {
			t.Fatalf("the full pinned namespace must resolve too: %v", err)
		}
	}
}

// The remedy the #2901 diagnostic prescribes must PARSE. It interpolated the
// symbol with %q, so it emitted `use cluster.concepts.{ "widget" }` -- quotes
// are not `use` syntax and no import anywhere in dsl/ carries them. The defect
// was general, not colon-specific: it applied to every pin, including the
// live dsl/deployment one.
func TestPinnedDomainRemedyIsValidUseSyntax(t *testing.T) {
	root := pinnedNamespaceWithRealDirTree()
	// Drop the import so the unimported-signature-concept diagnostic fires.
	root["deploy/queries.memql"] = file(`@enabled
@description("No import, so the ambiguity diagnostic fires with its remedy.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`)

	var remedy string
	for _, err := range loadTree(t, root).VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "Import it by its PINNED namespace") {
			remedy = err.Error()
		}
	}
	if remedy == "" {
		t.Skip("the pinned-ambiguity diagnostic did not fire on this fixture")
	}
	if strings.Contains(remedy, `{ "`) {
		t.Errorf("the prescribed remedy quotes the symbol name, which is not `use` syntax and "+
			"fails to parse if copy-pasted -- every import in dsl/ is unquoted "+
			"(`use common.traits.{ isActiveRecord }`): %s", remedy)
	}
}

// The namespace test is a TIEBREAKER, not a hard filter -- and getting that
// wrong is how the first cut of this review over-corrected.
//
// resolveBareConceptNameWithNamespace matches the trailing segment first and
// returns immediately when exactly one concept in the tree carries the name;
// the ":"+hint+":" needle is never built. So a UNIQUE bare name binds
// regardless of the hint, and a lint that filters it rejects an import boot
// accepts. Index.ConceptDeclared states the same rule in this package ("a
// unique bare name resolves regardless of the hint"), so a hard filter here
// would put two contradictory models of one boot rule in one package.
func TestLane1_UniqueBareNameBindsRegardlessOfNamespace(t *testing.T) {
	root := fstest.MapFS{
		"deploy/namespace.pin": file("cluster\n"),
		// No @namespace, so this assembles v1:deploy:widget -- and it is the
		// ONLY widget in the tree, so boot binds it under any hint.
		"deploy/concepts.memql": file(`@version("1.0.0")
@description("Un-annotated decl in a pinned directory; unique in the tree.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("The pin target exists but declares no widget.")
concept gadget {
  label  string  @required @description("Label.")
}`),
		"deploy/queries.memql": file(`use cluster.concepts.{ widget }

@enabled
@description("Boot binds this: widget is unique, so the hint is not consulted.")
query widget deployWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}

	for _, err := range loadTree(t, root).VerifyReferentialIntegrity() {
		if strings.Contains(err.Error(), "use cluster.concepts") {
			t.Fatalf("the lint rejected an import boot BINDS. widget is unique in this tree, so "+
				"resolveBareConceptNameWithNamespace returns it on the trailing-segment match "+
				"without ever consulting the hint. The per-declaration namespace test must apply "+
				"only when the name is ambiguous -- otherwise it is a hard filter boot does not "+
				"have, and it contradicts Index.ConceptDeclared: %v", err)
		}
	}
}
