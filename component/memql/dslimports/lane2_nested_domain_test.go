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
//
// The namespace.pin below is load-bearing, not decoration. Without it the
// annotation diverges from the directory, AssembleConceptIdFromDeclInDir
// returns the #2614 mismatch error, and candidateConceptId drops the candidate
// as unusable -- so this test would still pass, but because the id could not be
// BUILT rather than because it was FOLLOWED. The pin is what dsl/deployment
// carries on disk; with it the assembly succeeds and yields v1:cluster:widget,
// which is the input this test means to exercise.
func TestLane2_PinnedNamespaceDivergenceFollowsTheDecl(t *testing.T) {
	// Fixture shared with TestLane2_PinnedDomainDiagnosticNamesAnActionableFix so
	// both tests provably exercise the identical shape.
	root := pinnedNamespaceTree()
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

// pinnedNamespaceTree is the fixture TestLane2_PinnedNamespaceDivergenceFollowsTheDecl
// builds, extracted so the diagnostic test below exercises the identical shape.
// deployment/ is pinned to cluster, so `widget` assembles to v1:cluster:widget
// while the file's ambient hint is "deployment"; other/ declares a second
// widget so the name is ambiguous.
func pinnedNamespaceTree() fstest.MapFS {
	return fstest.MapFS{
		"deployment/namespace.pin": file("cluster\n"),
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
}

// TestLane2_PinnedDomainDiagnosticNamesAnActionableFix pins memql#2901.
//
// The sibling test above asserts the diagnostic FIRES for a pinned domain.
// This asserts it says something the author can act on, which it previously
// did not: the generic remedy is "import it via a use declaration", and for a
// pinned domain two of the three spellings do not work.
//
//   - no import -- boot's hint is the file's last path segment, which cannot
//     match ":"+pin+":"; that IS the reported ambiguity;
//   - `use deployment.concepts.{ widget }` -- stripped by the same-domain-use
//     gate (memql#2617). Worse, it silences THIS lane while boot still cannot
//     bind, turning a loud boot failure into a green lint;
//   - `use cluster.concepts.{ widget }` -- the one that works. It names the
//     namespace the concept assembles under, which is what boot matches.
//
// So the message must name the pinned-namespace import specifically.
//
// This comment used to end "the only real fixes are rename or unpin", because
// lane 1 resolved that third spelling against the cluster/ DIRECTORY and
// rejected it whenever such a directory existed. memql#2945 ruled that model
// wrong -- a use path's leading segment is a namespace -- so the import is now
// always available and the sibling test below asserts exactly that.
//
// The finding itself must survive: boot genuinely refuses this binding, so
// suppressing it would take memqllint green on a bundle that crash-loops at
// boot. The first assertion below is what stops a future "fix" doing that.
func TestLane2_PinnedDomainDiagnosticNamesAnActionableFix(t *testing.T) {
	tree := loadTree(t, pinnedNamespaceTree())

	var got string
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			got = e.Error()
		}
	}
	if got == "" {
		t.Fatal("lane 2 did not report the pinned-domain ambiguity at all. The finding is a TRUE " +
			"POSITIVE -- boot refuses this binding -- so it must not be suppressed; only its remedy " +
			"was wrong. See TestLane2_PinnedNamespaceDivergenceFollowsTheDecl (memql#2901).")
	}

	if strings.Contains(got, "import it via a use declaration") {
		t.Errorf("lane 2 offered the generic import remedy for a PINNED domain, where no import "+
			"spelling exists: the same-domain form is stripped by the same-domain-use gate and the "+
			"pinned-namespace form resolves against a directory that does not declare the concept "+
			"(memql#2901).\n  got: %s", got)
	}

	// This fixture has NO cluster/ directory. A use path is a NAMESPACE hint
	// matched against canonical ids, not a directory lookup, so
	// `use cluster.concepts.{ widget }` lints clean and binds correctly at
	// boot. An earlier version of this diagnostic asserted the opposite --
	// that the import "resolves against cluster/, which declares no widget",
	// naming a directory that does not exist -- and sent the author to re-key
	// ids instead of adding one line. That is the same defect memql#2901 was
	// filed about, one level up, so it is pinned here.
	//
	// Since memql#2945 the absence of cluster/ is no longer what makes the
	// import work -- the sibling test proves it works with the directory
	// present too. The fixture keeps its shape because these two tests are a
	// matched pair on one fixture; what it isolates now is that the diagnostic
	// says the same thing either way.
	for _, want := range []string{
		"namespace.pin",        // names the mechanism
		`"cluster"`,            // names the pin value
		"v1:cluster:widget",    // what the concept actually assembles to
		`"deployment"`,         // the ambient hint that cannot match
		"use cluster.concepts", // the fix that actually works here
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pinned-domain diagnostic does not mention %q, which the author needs in order "+
				"to act on it.\n  got: %s", want, got)
		}
	}
	if strings.Contains(got, "Rename one of the colliding concepts") {
		t.Errorf("the diagnostic told the author to rename a concept, but `use cluster.concepts.{ "+
			"widget }` binds this and is a ONE-LINE fix -- with or without a cluster/ directory "+
			"(memql#2945). Sending someone to re-key ids when an import would do is worse than "+
			"the message this replaced.\n  got: %s", got)
	}
}

// TestLane2_PinnedDomainImportRemedyHoldsWhenThePinDirectoryExists is the
// other half, and memql#2945 INVERTED it.
//
// It used to assert the opposite. When the pin named a real directory that
// declared no such concept, lane 1 rejected `use <pin>.concepts.{ X }`, so the
// message said rename the concept or remove the pin and re-key every id under
// it -- a data migration. #2945 ruled that lane 1 was wrong to reject: a use
// path's leading segment is a NAMESPACE, boot binds the import, and a lint
// exists to predict boot. With lane 1 resolving a namespace to every domain
// that assembles under it, the import works here too and the one-line remedy
// is the only remedy.
//
// So the pair of tests still does the job it was written for -- stopping the
// diagnostic from collapsing into a claim that is false for half its inputs --
// except the two halves now agree on the remedy and differ only in whether the
// pin directory exists. That difference must stay invisible to the author,
// which is what the final assertion checks.
func TestLane2_PinnedDomainImportRemedyHoldsWhenThePinDirectoryExists(t *testing.T) {
	root := pinnedNamespaceTree()
	// Give the pin a real directory that declares something ELSE. Before
	// #2945 this alone flipped the remedy from "add one import line" to
	// "re-key every canonical id in the domain".
	root["cluster/concepts.memql"] = file(`@version("1.0.0")
@namespace("cluster")
@description("The pin target exists but declares no widget.")
concept gadget {
  label  string  @required @description("Label.")
}`)
	tree := loadTree(t, root)

	var got string
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			got = e.Error()
		}
	}
	if got == "" {
		t.Fatal("lane 2 stopped reporting the pinned-domain ambiguity once the pin directory existed")
	}
	if !strings.Contains(got, "Import it by its PINNED namespace") {
		t.Errorf("the diagnostic did not offer the pinned-namespace import even though lane 1 now "+
			"accepts it: `widget` is declared in deployment/, which assembles under \":cluster:\", "+
			"so `use cluster.concepts.{ widget }` resolves whether or not cluster/ declares it "+
			"(memql#2945).\n  got: %s", got)
	}
	if strings.Contains(got, "Rename one of the colliding concepts") {
		t.Errorf("the diagnostic prescribed rename-or-unpin -- re-keying every canonical id in the "+
			"domain -- for a case a one-line import fixes. That remedy was correct only while lane "+
			"1 wrongly rejected the import, and memql#2945 overturned that.\n  got: %s", got)
	}

	// The load-bearing assertion: following the advice must actually work.
	// A message that recommends an import the lint then rejects is the exact
	// defect #2945 was filed about, so assert the END STATE rather than the
	// wording.
	root["deployment/queries.memql"] = file(`use cluster.concepts.{ widget }

@enabled
@description("Binds widget by its PINNED namespace, as the diagnostic advises.")
query widget deploymentWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`)
	for _, e := range loadTree(t, root).VerifyReferentialIntegrity() {
		t.Errorf("the tree still does not lint clean after following the diagnostic's own remedy. "+
			"The message and the lanes must agree, or the author is sent in a circle "+
			"(memql#2945).\n  got: %s", e)
	}
}

// TestLane2_PinnedDomainWithTwoOwnCandidatesFallsBackToTheGenericRemedy pins
// the multi-candidate guard.
//
// The pinned message names a SINGLE assembled id. When the pinned domain
// declares the name twice, naming one of the two would be a guess, so the
// helper declines and the generic remedy is emitted instead. Without the
// guard the message confidently reports whichever candidate the map iteration
// reached first, which is not deterministic.
func TestLane2_PinnedDomainWithTwoOwnCandidatesFallsBackToTheGenericRemedy(t *testing.T) {
	root := pinnedNamespaceTree()
	// A SECOND widget inside the pinned domain, in its own file.
	root["deployment/more.memql"] = file(`@version("1.0.0")
@namespace("cluster")
@description("A second widget inside the pinned domain.")
concept widget {
  other  string  @required @description("Other.")
}`)
	tree := loadTree(t, root)

	// memql#3008 REPLACED the guard this test was written for, and the reason
	// is worth stating rather than just editing the assertion: the old rule
	// declined to name an id because "picking one of two would be a guess
	// decided by map iteration order". That is true when the two candidates
	// assemble to DIFFERENT ids. Here they do not -- both decls sit under the
	// pinned `cluster` namespace and both assemble to `v1:cluster:widget` --
	// so there is nothing to guess. What was an unnameable choice is a
	// nameable fact.
	//
	// The invariant this test protects is unchanged: a pinned domain declaring
	// the name twice must still be REPORTED, never silently resolved.
	var got string
	for _, e := range tree.VerifyReferentialIntegrity() {
		msg := e.Error()
		if strings.Contains(msg, "same canonical id") || strings.Contains(msg, "cannot disambiguate") {
			got = msg
		}
	}
	if got == "" {
		t.Fatal("lane 2 stopped reporting anything when the pinned domain declared the name twice")
	}
	if !strings.Contains(got, "same canonical id") {
		t.Errorf("two decls in ONE pinned domain collide on a single assembled id, so the report "+
			"must say so rather than offering the generic 'import it via a use declaration' "+
			"remedy -- an import cannot separate two decls that already share a namespace "+
			"(memql#3008).\n  got: %s", got)
	}
	for _, want := range []string{"v1:cluster:widget"} {
		if !strings.Contains(got, want) {
			t.Errorf("the duplicate report does not name %q; the author needs the id as well as "+
				"the files.\n  got: %s", want, got)
		}
	}
}
