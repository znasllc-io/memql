package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

// deprecation_window_version_test.go -- a form's DeprecatedIn must name the
// release that will actually carry its warning (memql#5390).
//
// # The failure this exists to prevent
//
// `DeprecatedIn` is a literal, and the second release is derived from it
// (`RefusedFrom` = DeprecatedIn + MinimumMinorReleases). That makes the two
// dates internally consistent and says nothing about whether the FIRST one is
// true. A form written on a tree at 0.22.8 is correctly dated 0.23.0; if the
// change carrying it sits unmerged until 0.24 is cut, the page still publishes
// "deprecated in 0.23.0" -- a release that never warned about anything -- while
// the refusal still lands at 0.25. Operators then get ONE minor of warning
// instead of the two the page promises, and every other test in this change
// stays green, because every other test reads DeprecatedIn and agrees with it.
//
// The promise is two minors of warning. A date that predates the code is that
// promise broken on the very page that states it.
//
// # Why VERSION is the right thing to read
//
// VERSION carries the plain semver the next release will be cut at
// (VERSIONING.md), so it is the one fact in the tree that says which release
// this code will first appear in. The BINARY never reads it (memql#3998) and
// this does not change that: a test is not a binary, and nothing at run time
// consults the file.
//
// # Why there is a ledger
//
// Once a form's warning has shipped, its DeprecatedIn is HISTORY and VERSION
// has moved past it, so holding it to the tree's next minor would go red
// forever on a fact that is correct. Nothing in the tree distinguishes "shipped
// in 0.23.0" from "claims 0.23.0 and is landing in 0.24" -- only the release
// history does -- so that one bit is recorded here, once, by the release that
// ships it. An unrecorded form is held to the tree; a recorded one is checked
// for agreement instead.

// releasedForms names every form whose deprecation warning has ALREADY SHIPPED
// in a cut release, mapped to the release it first warned in -- which must be
// the form's own DeprecatedIn.
//
// EMPTY TODAY, and that is the load-bearing statement: no release has yet
// carried the deprecation window, so every registered form is still held to
// this tree's next minor. Add an entry only when the release naming it has
// actually been cut; the gate below refuses an entry for a release this tree
// has not reached, so the line cannot be written ahead of the fact.
var releasedForms = map[string]string{}

// versionFile is the tree's VERSION, at the repo root beside this test.
const versionFile = "VERSION"

// TestEveryDeprecatedFormIsDatedToTheReleaseThatWillCarryIt is the gate.
func TestEveryDeprecatedFormIsDatedToTheReleaseThatWillCarryIt(t *testing.T) {
	raw, err := os.ReadFile(versionFile)
	if err != nil {
		t.Fatalf("reading %s: %v", versionFile, err)
	}
	version := strings.TrimSpace(string(raw))
	next, ok := nextMinorRelease(version)
	if !ok {
		t.Fatalf("%s reads %q, which is not a MAJOR.MINOR.PATCH release", versionFile, version)
	}

	for _, f := range deprecation.Forms() {
		dep, ok := minorOf(f.DeprecatedIn)
		if !ok {
			t.Errorf("form %s: DeprecatedIn %q is not a readable release", f.Rule, f.DeprecatedIn)
			continue
		}
		if shipped, recorded := releasedForms[f.Rule]; recorded {
			// History. Two things are still checkable: the ledger must agree
			// with the form, and it must not name a release this tree has not
			// reached -- a line written ahead of the fact would exempt a form
			// from the gate before the release that earns the exemption.
			if shipped != f.DeprecatedIn {
				t.Errorf("form %s: releasedForms records %q but the form says it was deprecated in %q; "+
					"the ledger records the release that actually carried the warning, so the two cannot differ",
					f.Rule, shipped, f.DeprecatedIn)
			}
			if cur, ok := minorOf(version); ok && dep.after(cur) {
				t.Errorf("form %s is recorded in releasedForms as having shipped in %s, but this tree is at %s -- "+
					"a release that has not been cut cannot have carried a warning. Remove the ledger entry.",
					f.Rule, f.DeprecatedIn, version)
			}
			continue
		}
		if dep != next {
			t.Errorf(`form %s is dated to %s, but this tree cuts %s next.

DeprecatedIn must name the release that will FIRST carry this form's warning, because
that release is also what the language reference publishes and what RefusedFrom counts
%d minor releases from. A form dated earlier than the release that carries it gives
operators fewer than the %d minors of warning the page promises; a form dated later
warns before the date it publishes.

  VERSION          %s
  next minor       %s
  %s   %s  <- change this to %s.0

Changing DeprecatedIn moves the refusal with it: RefusedFrom is derived, so %s.0
refuses at %s rather than at %s. If instead this form's warning has genuinely
already shipped in %s, record that in releasedForms above -- that is the one case
this gate cannot see for itself.`,
				f.Rule, f.DeprecatedIn, next,
				deprecation.MinimumMinorReleases, deprecation.MinimumMinorReleases,
				version, next,
				f.Rule, f.DeprecatedIn, next,
				next, next.plusMinors(deprecation.MinimumMinorReleases), f.RefusedFrom(),
				f.DeprecatedIn)
		}
	}
}

// minorRelease is a MAJOR.MINOR point, the resolution a window is counted in.
type minorRelease struct{ major, minor int }

func (r minorRelease) String() string { return fmt.Sprintf("%d.%d", r.major, r.minor) }

func (r minorRelease) after(o minorRelease) bool {
	if r.major != o.major {
		return r.major > o.major
	}
	return r.minor > o.minor
}

func (r minorRelease) plusMinors(n int) minorRelease {
	return minorRelease{major: r.major, minor: r.minor + n}
}

// minorOf reads the MAJOR.MINOR of a release string, discarding the patch as
// the window does.
func minorOf(v string) (minorRelease, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 {
		return minorRelease{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return minorRelease{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return minorRelease{}, false
	}
	return minorRelease{major: major, minor: minor}, true
}

// nextMinorRelease is the next MINOR release this tree will cut, read off
// VERSION.
//
// VERSION is "the plain semver the next release will be cut at" (VERSIONING.md),
// so the patch is what decides: a VERSION ending in .0 IS the minor release
// being prepared, and any other patch means the next release is a patch of the
// current minor and the next MINOR is the one after it. Getting this backwards
// would fail the gate on the very commit that prepares the release carrying a
// form -- the one moment the date is most certainly right.
func nextMinorRelease(version string) (minorRelease, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	if len(parts) != 3 {
		return minorRelease{}, false
	}
	at, ok := minorOf(version)
	if !ok {
		return minorRelease{}, false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil || patch < 0 {
		return minorRelease{}, false
	}
	if patch == 0 {
		return at, true
	}
	return at.plusMinors(1), true
}

// The arithmetic above decides whether a date is a lie, so it is tested rather
// than trusted -- particularly the .0 case, which is the release-prep commit.
func TestNextMinorReleaseReadsVersionTheWayVersioningMdDefinesIt(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
		ok      bool
	}{
		{"0.22.8", "0.23", true},  // a patch is next, so the next MINOR is the one after
		{"0.23.0", "0.23", true},  // the minor itself is being prepared
		{"0.23.1", "0.24", true},  // it shipped; the next minor is the one after
		{"1.0.0", "1.0", true},    //
		{"1.4.12", "1.5", true},   //
		{"v0.22.8", "0.23", true}, // a v-prefixed tag reads the same
		{"0.22", "", false},       // not a release: the patch decides, so it must be there
		{"dev", "", false},
		{"", "", false},
	} {
		got, ok := nextMinorRelease(tc.version)
		if ok != tc.ok {
			t.Errorf("nextMinorRelease(%q) ok = %v, want %v", tc.version, ok, tc.ok)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("nextMinorRelease(%q) = %s, want %s", tc.version, got, tc.want)
		}
	}
}

// The gate is only worth having if it FAILS on the date it exists to catch, so
// the arithmetic is exercised against the exact scenario the review described:
// a form dated 0.23.0 on a tree that is already cutting 0.24.
func TestTheVersionGateRejectsAFormDatedBeforeTheReleaseThatCarriesIt(t *testing.T) {
	next, ok := nextMinorRelease("0.24.1")
	if !ok {
		t.Fatal("0.24.1 is a release")
	}
	dated, ok := minorOf("0.23.0")
	if !ok {
		t.Fatal("0.23.0 is a release")
	}
	if dated == next {
		t.Fatal("a form dated 0.23.0 must not satisfy a tree cutting 0.25 next: " +
			"that is the landing-late case, and it is the whole reason this gate exists")
	}
	// And the same form on the tree it was written for is accepted, or the gate
	// would be one that fails on everything.
	written, _ := nextMinorRelease("0.22.8")
	if dated != written {
		t.Fatalf("a form dated 0.23.0 on a tree at 0.22.8 must be accepted; the gate wants %s", written)
	}
}
