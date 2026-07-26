package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lane2_nested_domain_test.go -- memql#2852.
//
// The existing lane-2 fixture (lane2_same_domain_test.go) is entirely FLAT --
// alpha/concepts.memql, alpha/queries.memql. There was no nested fixture
// anywhere, which is why a fix that got the nested case wrong in BOTH
// directions passed the whole suite and left `go run ./cmd/memqllint dsl/`
// clean: the engine tree has no <domain>/<sub>/concepts.memql, and its 23
// nested files all carry explicit imports that short-circuit resolution before
// the domain comparison is reached.
//
// So these two fixtures pin the <domain>/<sub>/ layout, one per direction. Both
// FAIL against a rule that reduces the candidate side to its last directory
// segment, which is what the first cut of #2852 did.

// nestedDeclarationTree declares `widget` in a SUBDIRECTORY of beta, and again
// in gamma. beta's own query imports nothing.
//
// Boot resolves it: the query's namespace hint is "beta" (last directory
// segment of beta/queries.memql) and beta's concept assembles to v1:beta:widget
// under its FIRST path segment, so ":beta:" matches.
func nestedDeclarationTree() fstest.MapFS {
	return fstest.MapFS{
		// NO @namespace, deliberately: the id is then derived from the
		// directory, which is what makes this fixture discriminate. With an
		// explicit @namespace the #2614 mismatch guard errors on a wrong dir
		// and the annotation fallback silently rescues it -- so an
		// annotated fixture passes even against the broken rule.
		"beta/sub/concepts.memql": file(`@version("1.0.0")
@description("Beta's widget, declared in a subdirectory; namespace from the directory.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"gamma/concepts.memql": file(`@version("1.0.0")
@namespace("gamma")
@description("Gamma's widget -- same short name, different namespace.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"beta/queries.memql": file(`@enabled
@description("Binds beta's own widget with no import -- ambient under #2617.")
query widget betaWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}
}

// TestLane2_NestedDeclarationStillResolvesFromItsOwnNamespace is the
// FALSE-POSITIVE direction.
//
// Reducing the candidate to its last directory segment gives "sub", which does
// not match the file's hint "beta", so lane 2 reports ambiguity for a binding
// boot resolves. And the import that would silence it -- `use
// beta.sub.concepts.{ widget }` -- is stripped by TestNoSameDomainUse, whose
// domain rule also says "beta". So the bundle has NO spelling that passes both
// gates: #2805's unsatisfiable-lint shape, re-created by the change meant to
// remove it.
func TestLane2_NestedDeclarationStillResolvesFromItsOwnNamespace(t *testing.T) {
	tree := loadTree(t, nestedDeclarationTree())
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			t.Errorf("lane 2 reported ambiguity for a binding boot RESOLVES. beta's widget is "+
				"declared in beta/sub/ but assembles to v1:beta:widget under its first path "+
				"segment, and the query's namespace hint is \"beta\" -- so boot matches. The "+
				"import that would silence this is the one TestNoSameDomainUse strips, leaving "+
				"the bundle unfixable (memql#2852 / #2805): %v", e)
		} else {
			t.Errorf("unexpected diagnostic: %v", e)
		}
	}
}

// crossNamespaceSameSubdirTree is the FALSE-NEGATIVE direction: two UNRELATED
// top-level namespaces that happen to share a subdirectory NAME.
//
// alpha/sub/queries.memql binds `widget`; the declarations live in beta/sub/
// and gamma/. Boot refuses this -- the hint is "sub", and neither v1:beta:widget
// nor v1:gamma:widget contains ":sub:" -- so it is genuinely ambiguous.
//
// A last-segment rule on both sides sees "sub" == "sub" and resolves, so the
// lint goes GREEN on a bundle that crash-loops every node at boot. That is the
// worse direction: memqllint is the only pre-boot gate a product bundle has
// (CI's engine-load check runs against the embedded engine tree only).
func crossNamespaceSameSubdirTree() fstest.MapFS {
	return fstest.MapFS{
		// Directory-derived, as above -- this is the discriminating shape.
		"beta/sub/concepts.memql": file(`@version("1.0.0")
@description("Beta's widget; namespace from the directory.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"gamma/concepts.memql": file(`@version("1.0.0")
@namespace("gamma")
@description("Gamma's widget.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"alpha/sub/queries.memql": file(`@enabled
@description("Binds an ambiguous FOREIGN name with no import -- unresolvable.")
query widget alphaWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}
}

func TestLane2_SharedSubdirectoryNameIsNotSharedNamespace(t *testing.T) {
	tree := loadTree(t, crossNamespaceSameSubdirTree())

	var reported bool
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			reported = true
		}
	}
	if !reported {
		t.Error("lane 2 was SILENT on a genuinely ambiguous binding. alpha/sub/queries.memql binds " +
			"`widget`, declared only in beta/sub/ and gamma/ -- two unrelated top-level " +
			"namespaces. Boot refuses it (\"ambiguous concept name\"), so a green lint here means " +
			"the bundle crash-loops every node at boot, and memqllint is the only pre-boot gate a " +
			"product bundle has. A rule that compares last-directory-segments sees \"sub\" == " +
			"\"sub\" and resolves (memql#2852).")
	}
}

// TestLane2_PinnedNamespaceDivergenceFollowsTheDecl covers the half #2852 names
// and the first cut ignored: a domain whose concepts carry @namespace(...)
// pointing somewhere other than their directory.
//
// dsl/deployment is the live instance -- its concepts declare
// @namespace("cluster") and assemble to v1:cluster:deployment. A rule comparing
// DIRECTORY strings says deployment == deployment and resolves; boot's hint is
// "deployment" and the id contains ":cluster:", so it does NOT. Following the
// decl's assembled id is what makes the two agree.
func TestLane2_PinnedNamespaceDivergenceFollowsTheDecl(t *testing.T) {
	root := fstest.MapFS{
		"deployment/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Declared under deployment/, namespaced to cluster.")
concept widget {
  label  string  @required @description("Label.")
}`),
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
@description("A second widget so the name is ambiguous.")
concept widget {
  name  string  @required @description("Name.")
}`),
		"deployment/queries.memql": file(`@enabled
@description("Binds ` + "`widget`" + ` with no import from deployment/.")
query widget deploymentWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	}
	tree := loadTree(t, root)

	var reported bool
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			reported = true
		}
	}
	if !reported {
		t.Error("lane 2 resolved a binding boot does NOT. The concept is declared under " +
			"deployment/ but carries @namespace(\"cluster\"), so it assembles to " +
			"v1:cluster:widget; the query's namespace hint is \"deployment\", which does not " +
			"match. Comparing DIRECTORY strings instead of the decl's assembled id makes the " +
			"lint assert an ambient resolution the engine cannot perform (memql#2852).")
	}
}
