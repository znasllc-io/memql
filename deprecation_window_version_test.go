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
// VERSION equals the tag of the commit a release cut tags (VERSIONING.md,
// memql#5714): between cuts it names the release main was last cut at, and a
// "prepare" pull request moves it to the next one immediately before the cut.
// Either way, code not yet released first appears in a LATER release than the
// one VERSION names, so VERSION is the one fact in the tree that bounds which
// release this code will first appear in. The BINARY never reads it
// (memql#3998) and this does not change that: a test is not a binary, and
// nothing at run time consults the file.
//
// # Why there is a ledger
//
// Once a form's warning has shipped, its DeprecatedIn is HISTORY and VERSION
// has moved past it, so holding it to the tree's next minor would go red
// forever on a fact that is correct. Nothing in the tree distinguishes "shipped
// in 0.23.0" from "claims 0.23.0 and is landing in 0.24" -- only the release
// history does -- so that one bit is recorded here, once, by the release that
// ships it: the prepare pull request that sets VERSION to X.Y.0 records every
// form dated X.Y.0 in the same change, because that is the moment the tree
// stops being able to cut X.Y.0 for anything new. An unrecorded form is held to
// the tree; a recorded one is checked for agreement instead.

// releasedForms names every form whose deprecation warning is FIXED IN A
// RELEASE, mapped to the release it first warns in -- which must be the form's
// own DeprecatedIn. That is a release already cut, or the X.Y.0 a merged
// prepare pull request is about to cut: the prepare pull request records the
// forms it carries in the same change that sets VERSION, because from that
// commit on the tree can no longer tell "ships in X.Y.0" from "dated X.Y.0 and
// landing in a later minor". Either way the entry never names a release beyond
// VERSION.
//
// v0.23.0 (da2222f4778278fdf955758c005f38d1ecc8ff43) shipped this
// warning. Its original deprecation/refusal dates remain historical facts.
var releasedForms = map[string]string{
	"deprecated_array_type":    "0.23.0",
	"deprecated_allowed_roles": "0.24.0",
}

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
			t.Error(misdatedFormMessage(f, version, next))
		}
	}
}

// misdatedFormMessage explains a form dated to a release other than the one
// that will first carry it, and says what to change.
//
// ONE CASE LEADS WITH THE LEDGER. When VERSION is X.Y.0 and the form is dated
// X.Y.0, two different commits look identical from here: the prepare pull
// request for X.Y.0, whose forms ship in X.Y.0 and are recorded in
// releasedForms; and a form written after X.Y.0 was cut, which first ships in
// the next minor and must be re-dated. The first is the one that meets this
// message on every minor prepare pull request carrying a form, and following
// the re-dating advice there would publish a date one minor late and delay the
// refusal with it -- so that case names the ledger first and the re-date second.
func misdatedFormMessage(f deprecation.Form, version string, next minorRelease) string {
	redate := fmt.Sprintf(`DeprecatedIn must name the release that will FIRST carry this form's warning, because
that release is also what the language reference publishes and what RefusedFrom counts
%d minor releases from. A form dated earlier than the release that carries it gives
operators fewer than the %d minors of warning the page promises; a form dated later
warns before the date it publishes.

  VERSION          %s
  next minor       %s
  %s   %s  <- change this to %s.0

Changing DeprecatedIn moves the refusal with it: RefusedFrom is derived, so %s.0
refuses at %s rather than at %s.`,
		deprecation.MinimumMinorReleases, deprecation.MinimumMinorReleases,
		version, next,
		f.Rule, f.DeprecatedIn, next,
		next, next.plusMinors(deprecation.MinimumMinorReleases), f.RefusedFrom())

	if isPreparePullRequestDate(f.DeprecatedIn, version) {
		return fmt.Sprintf(`form %s is dated to %s, and VERSION reads %s.

If this is the prepare pull request for %s -- the change that sets VERSION to it --
this form's warning ships in %s and the date is right: record it in releasedForms
above,

  %q: %q,

The prepare pull request owes that entry for every form its release is the first
to carry, because from this commit on nothing in the tree tells "ships in %s"
apart from "dated %s and landing later".

Otherwise %s is already cut and this form is not in it, so it first ships in
%s.0:

%s`,
			f.Rule, f.DeprecatedIn, version,
			version, version, f.Rule, f.DeprecatedIn,
			version, version,
			version, next,
			redate)
	}

	return fmt.Sprintf(`form %s is dated to %s, but this tree cuts %s next.

%s

If instead this form's warning has genuinely already shipped in %s, record that in
releasedForms above; that is the one case this gate cannot see for itself.`,
		f.Rule, f.DeprecatedIn, next, redate, f.DeprecatedIn)
}

// isPreparePullRequestDate reports whether a form dated deprecatedIn could be
// carried by the release a merged prepare pull request is about to cut: VERSION
// is an X.Y.0 and the form is dated to that same X.Y.
func isPreparePullRequestDate(deprecatedIn, version string) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	if len(parts) != 3 || parts[2] != "0" {
		return false
	}
	dep, ok := minorOf(deprecatedIn)
	if !ok {
		return false
	}
	cur, ok := minorOf(version)
	return ok && dep == cur
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

// nextMinorRelease is the first MINOR release that can carry code this tree
// adds, read off VERSION: always the minor after VERSION's own.
//
// VERSION equals the tag of the commit a cut tags (VERSIONING.md, memql#5714),
// so it names a release that is already cut -- or, for the few minutes between
// a merged prepare pull request and the cut, one whose contents are already
// fixed. Code not in that release lands in a later one, and the first later
// MINOR is VERSION's minor plus one, whatever the patch.
//
// It used to read the patch: a VERSION ending in .0 was taken to be the minor
// being PREPARED, because VERSION was then described as "the release the next
// cut will be". Under the tag-equality rule a .0 VERSION is, nearly always, a
// minor that has ALREADY SHIPPED, and that reading told a form written the day
// after 0.24.0 was cut to date itself 0.24.0 -- the exact landing-late date this
// gate exists to refuse. The prepare pull request for X.Y.0 now records the
// forms X.Y.0 carries in releasedForms, which is the ledger's own rule.
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
	return at.plusMinors(1), true
}

// The arithmetic above decides whether a date is a lie, so it is tested rather
// than trusted -- particularly the .0 case, which is a minor that has shipped
// (or whose prepare pull request has fixed its contents), not one still open.
func TestNextMinorReleaseReadsVersionTheWayVersioningMdDefinesIt(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
		ok      bool
	}{
		{"0.22.8", "0.23", true},  // 0.22.8 is cut; new code lands in 0.22.9 or 0.23.0
		{"0.23.0", "0.24", true},  // 0.23.0 is cut (or prepared); new code misses it
		{"0.23.1", "0.24", true},  //
		{"1.0.0", "1.1", true},    //
		{"1.4.12", "1.5", true},   //
		{"v0.22.8", "0.23", true}, // a v-prefixed tag reads the same
		{"0.22", "", false},       // not a release: VERSION is always X.Y.Z
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

// The prepare pull request for a minor is the one commit that meets this gate
// with a correctly dated form every time it carries one, so its message must
// lead with the ledger rather than with a re-date that would publish the date
// one minor late. Every other misdating keeps the re-date as its lead.
func TestTheMisdatedFormMessageLeadsWithTheFixThatFits(t *testing.T) {
	form := deprecation.Form{Rule: "acme_form", DeprecatedIn: "0.24.0"}

	next, _ := nextMinorRelease("0.24.0")
	prepare := misdatedFormMessage(form, "0.24.0", next)
	ledger := strings.Index(prepare, "record it in releasedForms")
	redate := strings.Index(prepare, "<- change this to 0.25.0")
	if ledger < 0 || redate < 0 || ledger > redate {
		t.Errorf("on the prepare pull request for 0.24.0 the ledger must come first and the re-date second:\n%s", prepare)
	}
	if !strings.Contains(prepare, `"acme_form": "0.24.0"`) {
		t.Errorf("the prepare-case message does not spell the ledger entry to add:\n%s", prepare)
	}

	next, _ = nextMinorRelease("0.24.1")
	late := misdatedFormMessage(form, "0.24.1", next)
	if strings.Contains(late, "prepare pull request for") {
		t.Errorf("0.24.1 has no prepare pull request for 0.24.0 left to be; the message must not offer one:\n%s", late)
	}
	if !strings.HasPrefix(late, "form acme_form is dated to 0.24.0, but this tree cuts 0.25 next.") {
		t.Errorf("a form landing after its release was cut must lead with the re-date:\n%s", late)
	}
}

func TestIsPreparePullRequestDate(t *testing.T) {
	for _, tc := range []struct {
		deprecatedIn, version string
		want                  bool
	}{
		{"0.24.0", "0.24.0", true},   // the prepare pull request for 0.24.0
		{"0.24.0", "v0.24.0", true},  // either spelling of VERSION
		{"0.24.0", "0.24.1", false},  // 0.24.0 is behind the tree
		{"0.25.0", "0.24.0", false},  // the tree's next minor: the gate accepts it without help
		{"0.23.0", "0.24.0", false},  // an earlier minor
		{"0.24.0", "0.24", false},    // not a release
		{"garbage", "0.24.0", false}, // not a date
	} {
		if got := isPreparePullRequestDate(tc.deprecatedIn, tc.version); got != tc.want {
			t.Errorf("isPreparePullRequestDate(%q, %q) = %v, want %v", tc.deprecatedIn, tc.version, got, tc.want)
		}
	}
}
