package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// differing_id_concept_decl_test.go -- memql#3073.
//
// memql#3008 closed the case where two directories in one namespace declare the
// same concept and the declarations assemble to the SAME canonical id. When
// they assemble to DIFFERENT ids -- a differing @version is enough -- nothing
// changed: resolveConceptForFile still took hits[0], and lanes 3/5/6 validated
// fields against whichever directory sorted first.
//
// That reproduces #3008's original symptom verbatim: one spelling of a CORRECT
// query is told its field does not exist, and the mirror gets zero diagnostics.
// The verdict turns on directory sort order rather than on the tree being
// wrong.

// differingIdAcrossDirectoriesTree is duplicateAcrossDirectoriesTree with one
// byte changed: the deploy declaration carries @version("2.0.0"), so the two
// assemble to different canonical ids and the #3008 collision check does not
// fire. Everything else -- the pin, the namespaces, the consumer import -- is
// identical, which is what makes it the exact residue that issue left.
func differingIdAcrossDirectoriesTree(consumerField string) fstest.MapFS {
	return fstest.MapFS{
		"deploy/namespace.pin": file("cluster\n"),
		"deploy/concepts.memql": file(`@version("2.0.0")
@namespace("cluster")
@description("Declared under deploy/, pinned to cluster, at a different version.")
concept widget {
  fromDeploy  string  @required @description("Only on the deploy declaration.")
}`),
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Declared under cluster/, the real directory.")
concept widget {
  fromCluster  string  @required @description("Only on the cluster declaration.")
}`),
		"consumer/queries.memql": file(`use cluster.concepts.{ widget }

@enabled
@description("Filters on a field one declaration has.")
query widget consumerWidgets {
  args {
    v  string  @required
  }
  filter  ` + consumerField + ` == args.v
}`),
	}
}

// The trap #3008 named: assert BOTH mirrors, because exactly one of them passes
// under positional resolution whichever way the sort falls. A test driving one
// spelling proves nothing -- it is 50/50 that it happens to pick the lucky one.
func TestDifferingIdConceptDeclsAreReportedNotResolvedPositionally(t *testing.T) {
	for _, field := range []string{"fromDeploy", "fromCluster"} {
		t.Run("consumer filters on "+field, func(t *testing.T) {
			joined := treeErrorText(t, differingIdAcrossDirectoriesTree(field))

			// Whichever field the consumer names, it is a real field on one of
			// the two declarations. So the "does not declare" diagnostic is
			// ALWAYS wrong here -- it is the symptom, not the finding.
			if strings.Contains(joined, "does not declare") {
				t.Errorf("the misleading #3008 symptom still fires for the differing-id shape.\n"+
					"The consumer filters on %q, which IS declared -- on the other declaration. "+
					"Telling the author their field does not exist is wrong about their code, "+
					"not merely unhelpful (memql#3073).\n  got:\n%s", field, joined)
			}

			// And the tree defect itself must be reported, in BOTH mirrors --
			// otherwise one spelling is silently accepted and the author never
			// learns the namespace is supplied twice.
			if !mentionsDifferingIdAmbiguity(joined) {
				t.Errorf("no diagnostic reported the two declarations supplying namespace "+
					"%q for %q at different canonical ids. Positional resolution leaves one "+
					"mirror completely silent (memql#3073).\n  got:\n%s", "cluster", "widget", joined)
			}
		})
	}
}

// mentionsDifferingIdAmbiguity looks for the finding rather than for exact
// prose, so the message can be reworded without rewriting the test.
func mentionsDifferingIdAmbiguity(joined string) bool {
	return strings.Contains(joined, "widget") &&
		(strings.Contains(joined, "different canonical id") ||
			strings.Contains(joined, "cannot select") ||
			strings.Contains(joined, "supplied by"))
}

// The finding must be IDENTICAL for both mirrors. A diagnostic whose text
// depends on which directory sorted first is the same positional dependence
// wearing a different hat.
func TestDifferingIdDiagnosticDoesNotDependOnSortOrder(t *testing.T) {
	a := treeErrorText(t, differingIdAcrossDirectoriesTree("fromDeploy"))
	b := treeErrorText(t, differingIdAcrossDirectoriesTree("fromCluster"))
	if a != b {
		t.Errorf("the diagnostics differ between the two mirrors, so the verdict still "+
			"depends on sort order rather than on the tree.\n  fromDeploy:\n%s\n\n  fromCluster:\n%s", a, b)
	}
}

// The same-id case must keep its OWN message. #3008's reasoning is that an
// import cannot help there -- the decls already share a namespace AND an id --
// so it names both files and the id and says to rename one. This change must
// not collapse the two findings into one weaker message.
func TestSameIdCollisionKeepsItsOwnDiagnostic(t *testing.T) {
	joined := treeErrorText(t, duplicateAcrossDirectoriesTree("fromDeploy"))
	if !strings.Contains(joined, "same canonical id") {
		t.Errorf("the #3008 same-id diagnostic was lost or reworded into the #3073 one; "+
			"they are different defects with different remedies.\n  got:\n%s", joined)
	}
}

// treeErrorText loads a fixture and joins every referential-integrity
// diagnostic, so a test can assert on the whole report rather than on an index
// into it.
func treeErrorText(t *testing.T, fs fstest.MapFS) string {
	t.Helper()
	tree := loadTree(t, fs)
	var msgs []string
	for _, e := range tree.VerifyReferentialIntegrity() {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}
