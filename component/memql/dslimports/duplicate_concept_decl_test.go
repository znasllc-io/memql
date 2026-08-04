package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// memql#3008: two directories in one namespace declaring the same concept.
//
// The fixture is the issue's own, and it reproduces both halves of the defect:
// `deploy/` is pinned to `cluster`, a real `cluster/` directory exists, both
// declare `widget` with DIFFERENT properties, and a consumer imports
// `use cluster.concepts.{ widget }` and filters on a field only one of them has.
//
// Before this change `resolveConceptForFile` took the FIRST target declaring
// the name, in positional order, and lanes 3/5/6 validated fields against that
// arbitrary pick. The visible symptom was worse than a wrong pick: an author
// with a correct query got told their field "does not exist", because it
// existed on the OTHER declaration. The verdict depended on which directory
// sorted first, not on the tree being wrong.
func duplicateAcrossDirectoriesTree(consumerField string) fstest.MapFS {
	return fstest.MapFS{
		"deploy/namespace.pin": file("cluster\n"),
		"deploy/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Declared under deploy/, pinned to cluster.")
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

func TestDuplicateConceptAcrossDirectoriesIsReportedNotResolved(t *testing.T) {
	// BOTH mirrors. The issue's measurement was that one field spelling
	// produced a confident "does not declare" diagnostic and the other produced
	// zero -- purely on directory ordering. Asserting only one would leave the
	// gate passing for whichever way the sort happened to fall.
	for _, field := range []string{"fromDeploy", "fromCluster"} {
		t.Run(field, func(t *testing.T) {
			tree := loadTree(t, duplicateAcrossDirectoriesTree(field))

			var dup, misleading []string
			for _, e := range tree.VerifyReferentialIntegrity() {
				msg := e.Error()
				if strings.Contains(msg, "same canonical id") {
					dup = append(dup, msg)
				}
				if strings.Contains(msg, "does not declare") {
					misleading = append(misleading, msg)
				}
			}

			if len(dup) == 0 {
				t.Fatalf("two directories in namespace `cluster` both declare `widget` and both "+
					"assemble to v1:cluster:widget, but no duplicate was reported. Boot's registry "+
					"is keyed by canonical id, so these collapse to one row and the last "+
					"registration silently wins -- nothing else in the tree reports it "+
					"(memql#3008). Filtering on %q.", field)
			}

			joined := strings.Join(dup, "\n")
			for _, want := range []string{
				"cluster/concepts.memql", // both files, named
				"deploy/concepts.memql",
				"v1:cluster:widget", // and the id they collide on
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("the duplicate report does not name %q. The author needs to know "+
						"WHICH two declarations to reconcile; 'ambiguous' is not actionable.\n"+
						"  got: %s", want, joined)
				}
			}

			// The second half of the DoD, and the reason the issue was filed
			// rather than shrugged at: the misleading diagnostic must be gone.
			// Sending an author to fix a query that is correct costs more than
			// reporting nothing.
			if len(misleading) > 0 {
				t.Errorf("still emitting a 'field does not declare' diagnostic while the concept "+
					"has two colliding declarations. That sends the author to fix a query that is "+
					"correct against one of them (memql#3008).\n  got: %s",
					strings.Join(misleading, "\n"))
			}
		})
	}
}

// The converse: two decls of one name in DIFFERENT namespaces are a genuine
// ambiguity, not a duplicate, and must keep their own diagnostic.
//
// Pinned because the duplicate check runs BEFORE the ambiguity branch, and a
// collision test that is too eager would swallow the ambiguity report -- the
// two conditions look similar and have opposite remedies. An import fixes an
// ambiguity; nothing an author writes at the call site fixes a collision.
func TestDistinctNamespacesStillReportAmbiguityNotDuplication(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"alpha/concepts.memql": file(`@version("1.0.0")
@namespace("alpha")
@description("alpha widget.")
concept widget {
  a  string  @required @description("A.")
}`),
		"beta/concepts.memql": file(`@version("1.0.0")
@namespace("beta")
@description("beta widget.")
concept widget {
  b  string  @required @description("B.")
}`),
		"consumer/queries.memql": file(`@enabled
@description("Binds widget with NO import, from a third domain.")
query widget consumerWidgets {
  args {
    v  string  @required
  }
  filter  a == args.v
}`),
	})

	var sawAmbiguous, sawDuplicate bool
	for _, e := range tree.VerifyReferentialIntegrity() {
		msg := e.Error()
		if strings.Contains(msg, "cannot disambiguate") {
			sawAmbiguous = true
		}
		if strings.Contains(msg, "same canonical id") {
			sawDuplicate = true
		}
	}
	if sawDuplicate {
		t.Error("two decls in DIFFERENT namespaces were reported as a same-id duplicate. They " +
			"assemble to v1:alpha:widget and v1:beta:widget, which do not collide -- reporting " +
			"a collision here would give the wrong remedy and mask the real ambiguity.")
	}
	if !sawAmbiguous {
		t.Error("the genuine cross-namespace ambiguity stopped being reported. The duplicate " +
			"check runs first, so an over-eager collision test swallows this -- which is the " +
			"failure mode this assertion exists for.")
	}
}
