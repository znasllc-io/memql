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
	return differingIdTreeWithPinnedDir("deploy", consumerField)
}

// differingIdTreeWithPinnedDir is the same fixture with the PINNED directory's
// name as a parameter, so a test can move it across the "cluster" sort
// boundary: "aaa" sorts before, "zzz" after.
//
// It is the pinned directory that moves, not the real one. A namespace is only
// registered by a directory NAMED for it -- a pin alone does not create it --
// so renaming "cluster/" makes the namespace vanish and the fixture silently
// degrades to a one-supplier tree reporting nothing. The first attempt at this
// test did exactly that and asserted against an empty string.
func differingIdTreeWithPinnedDir(pinnedDir, consumerField string) fstest.MapFS {
	return fstest.MapFS{
		pinnedDir + "/namespace.pin": file("cluster\n"),
		pinnedDir + "/concepts.memql": file(`@version("2.0.0")
@namespace("cluster")
@description("Declared under the pinned directory, at a different version.")
concept widget {
  fromDeploy  string  @required @description("Only on the pinned declaration.")
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
			//
			// Assert the ACTIONABLE CONTENT, not that some phrase appeared. The
			// author has to know WHICH declarations to reconcile, so both files
			// and both ids are the point of the message -- exactly what the
			// sibling #3008 test asserts. An earlier version of this checked a
			// helper that accepted any ONE of three substrings; measured, that
			// let the whole diagnostic degrade to `"widget" cannot select` --
			// no file, no namespace, no id, no remedy -- with the suite green.
			for _, want := range []string{
				"cluster/concepts.memql",
				"deploy/concepts.memql",
				"v1:cluster:widget",
				"v2:cluster:widget",
				`namespace "cluster"`,
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("the diagnostic does not name %s, so it cannot be acted on. "+
						"The author needs both declarations and both assembled ids to know "+
						"which one to change (memql#3073).\n  got:\n%s", want, joined)
				}
			}
		})
	}
}

// The finding must not change when the supplying directory MOVES in sort order.
//
// This test used to compare the two consumer-field mirrors, which have
// identical directory layouts -- so it never varied sort order at all and could
// not have caught a regression in the property it is named for. Measured: with
// the sorts removed from splitNamespaceDecls it failed only 14 runs in 40, on
// Go's map-iteration randomness alone. What actually pins the property is
// moving the real directory across the "deploy" boundary, which is what this
// now does: "cluster" sorts before, "widgetry" after.
func TestDifferingIdDiagnosticIsOrderedAndStable(t *testing.T) {
	// The property is NOT "the message is unchanged when a directory is
	// renamed" -- renaming legitimately moves a file in a sorted list, and an
	// earlier draft of this test asserted that and failed against correct code.
	//
	// The property is that the listing is SORTED and therefore reproducible,
	// rather than reflecting whatever order a map happened to yield. That is
	// what makes the same tree give the same verdict on every run and on every
	// machine, which is the dependence memql#3073 is about.
	for _, tc := range []struct{ pinnedDir, first, second string }{
		{"aaa", "aaa/concepts.memql", "cluster/concepts.memql"},
		{"zzz", "cluster/concepts.memql", "zzz/concepts.memql"},
	} {
		t.Run(tc.pinnedDir, func(t *testing.T) {
			got := treeErrorText(t, differingIdTreeWithPinnedDir(tc.pinnedDir, "fromCluster"))
			if !strings.Contains(got, "different canonical ids") {
				t.Fatalf("the fixture stopped reporting the split, so the assertions below "+
					"would pass vacuously.\n  got:\n%s", got)
			}
			i, j := strings.Index(got, tc.first), strings.Index(got, tc.second)
			if i < 0 || j < 0 || i > j {
				t.Errorf("the two declarations are not listed in sorted order (%s before %s), "+
					"so the message reflects map iteration rather than the tree.\n  got:\n%s",
					tc.first, tc.second, got)
			}
			if !strings.Contains(got, "v1:cluster:widget and v2:cluster:widget") {
				t.Errorf("the assembled ids are not listed in sorted order.\n  got:\n%s", got)
			}
		})
	}

	// Sorted output is only worth anything if it is also stable run to run. Go
	// randomises map iteration per run, so repeating the identical tree is what
	// actually exercises that -- a single run of an unsorted implementation
	// passes roughly two times in three.
	first := treeErrorText(t, differingIdTreeWithPinnedDir("aaa", "fromCluster"))
	for i := 0; i < 50; i++ {
		if got := treeErrorText(t, differingIdTreeWithPinnedDir("aaa", "fromCluster")); got != first {
			t.Fatalf("the same tree produced two different diagnostics across runs, so the "+
				"report depends on map iteration order (memql#3073).\n  run 0:\n%s\n\n  run %d:\n%s",
				first, i+1, got)
		}
	}
}

// The two states must not cross-contaminate. #3008's remedy (rename one, they
// collapse at boot) and #3073's (they do NOT collapse; change what a directory
// supplies) are different advice, so a same-id tree must get the same-id
// message and NOT the split one.
//
// The positive half of this -- that "same canonical id" is still reported -- is
// already covered by TestDuplicateConceptAcrossDirectoriesIsReportedNotResolved,
// which asserts strictly more (both filenames and the id). Asserting it again
// here would add no coverage: no mutation can fail this without failing that.
// The ABSENCE half is what is unique, and it is the one that catches the split
// check being relaxed until it swallows the same-id case.
func TestSameIdCollisionDoesNotGetTheSplitDiagnostic(t *testing.T) {
	joined := treeErrorText(t, duplicateAcrossDirectoriesTree("fromDeploy"))
	if strings.Contains(joined, "assemble to different canonical ids") {
		t.Errorf("a SAME-id collision was reported with the memql#3073 split message. "+
			"Those declarations do collapse at boot, so the split remedy (change which "+
			"namespace a directory supplies) is wrong advice here -- the fix is to rename "+
			"one (memql#3008).\n  got:\n%s", joined)
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
