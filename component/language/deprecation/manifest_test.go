package deprecation_test

// The window's length is a MANIFEST VALUE, and it is written down twice on
// purpose (memql#5390). This test is what stops the second copy becoming a
// second answer.
//
// Why twice at all: component/language/tiers/limits.go is where this repository
// keeps the language's manifest values -- the static cost ceiling, the step
// budget, the collection-size estimate -- and its own comment has named "the
// deprecation window" in that list since the list was written. But the window
// is consulted from inside the LOADER, so every import the deprecation package
// carries is an import taken at load time, and a leaf with no dependencies is
// the shape that cannot introduce a cycle there.
//
// So the value is declared in both, and neither imports the other. A TEST can
// import both, which is the cheapest place to put the equality: a divergence is
// caught at `go test` with a message naming both files, rather than at a load
// nobody is watching, where the symptom would be a form that refuses two
// releases earlier or later than the release note says it will.

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
	"github.com/znasllc-io/memql/component/language/tiers"
)

func TestDeprecationWindowMatchesTheManifest(t *testing.T) {
	if deprecation.MinimumMinorReleases != tiers.DeprecationWindowMinorReleases {
		t.Fatalf("the deprecation window is written down twice and the two disagree:\n"+
			"  component/language/deprecation.MinimumMinorReleases      = %d\n"+
			"  component/language/tiers.DeprecationWindowMinorReleases  = %d\n\n"+
			"The second is the manifest value; the first is what the code reads. Move them together, "+
			"and move them only for a reason D22 admits -- \"at least two minor releases\" is a floor, "+
			"so lowering either below 2 is a change to the record, not to a constant.",
			deprecation.MinimumMinorReleases, tiers.DeprecationWindowMinorReleases)
	}
	if deprecation.MinimumMinorReleases < 2 {
		t.Fatalf("the deprecation window is %d minor releases; D22 sets the floor at two, and a shorter "+
			"window lets a form refuse before an author has had a release in which to see the warning",
			deprecation.MinimumMinorReleases)
	}
}
