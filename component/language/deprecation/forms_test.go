package deprecation

import (
	"strings"
	"testing"
)

// The registry of forms in a window (memql#5390; D22).
//
// window_test.go tests the ARITHMETIC over invented windows. This file tests
// the TABLE the arithmetic is applied to, and the three claims a shipped form
// has to make good on:
//
//   - the form is COMPLETE -- rule, spelling, replacement, migrator, and a
//     readable deprecation release -- because each of those is a half of the
//     message an author reads, and the one most often missing (the migrator) is
//     the one that does the work;
//   - its window is AT LEAST the record's minimum, computed rather than
//     asserted, so a form cannot be given a shorter one by writing a number
//     down;
//   - the refusal is a function of (form, release), tested at both ends of the
//     window without waiting for a release to ship.

func TestEveryShippedFormIsComplete(t *testing.T) {
	forms := Forms()
	if len(forms) == 0 {
		t.Fatal("no forms are registered; the registry exists to hold them, and an empty one " +
			"makes every consumer's handling of a deprecated form untested")
	}
	for _, f := range forms {
		for _, field := range []struct{ name, value string }{
			{"Rule", f.Rule},
			{"Spelling", f.Spelling},
			{"Replacement", f.Replacement},
			{"Migrator", f.Migrator},
			{"DeprecatedIn", f.DeprecatedIn},
		} {
			if strings.TrimSpace(field.value) == "" {
				t.Errorf("form %q: %s is empty", f.Rule, field.name)
			}
		}
		if _, ok := parseRelease(f.DeprecatedIn); !ok {
			t.Errorf("form %s: DeprecatedIn %q is not a readable release, so the window has no "+
				"start to count from and the form would warn forever", f.Rule, f.DeprecatedIn)
		}
		if got, ok := Lookup(f.Rule); !ok || got != f {
			t.Errorf("form %s is in Forms() but Lookup does not return it", f.Rule)
		}
	}
}

// The window is at least the record's minimum, and it is COMPUTED from the two
// releases rather than taken on trust: a form's expiry is derived, so the way
// this can go wrong is a DeprecatedIn that reads but does not mean what it says.
func TestEveryShippedFormGetsAtLeastTheMinimumWindow(t *testing.T) {
	for _, f := range Forms() {
		dep, ok := parseRelease(f.DeprecatedIn)
		if !ok {
			continue // reported by TestEveryShippedFormIsComplete
		}
		end, ok := parseRelease(f.RefusedFrom())
		if !ok {
			t.Errorf("form %s: RefusedFrom() = %q is not a readable release", f.Rule, f.RefusedFrom())
			continue
		}
		if end.major != dep.major {
			t.Errorf("form %s: the window crosses a major boundary (%s -> %s); the minors released "+
				"in between cannot be read off two versions, so pick a DeprecatedIn whose expiry stays "+
				"inside the major", f.Rule, f.DeprecatedIn, f.RefusedFrom())
			continue
		}
		if minors := end.minor - dep.minor; minors < MinimumMinorReleases {
			t.Errorf("form %s: a window from %s to %s is %d minor release(s); D22 sets the floor at %d",
				f.Rule, f.DeprecatedIn, f.RefusedFrom(), minors, MinimumMinorReleases)
		}
	}
}

// `array(T)` is the first form through the window, and its two releases are the
// arithmetic stated in forms.go: the tree's VERSION was 0.22.8, so the next
// minor is 0.23, and two minors later is 0.25.
func TestArrayTypeIsDeprecatedIn023AndRefusedFrom025(t *testing.T) {
	f, ok := Lookup(ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", ArrayType)
	}
	if f.Spelling != "array(T)" || f.Replacement != "[]T" {
		t.Errorf("array(T) -> []T is the form; got %q -> %q", f.Spelling, f.Replacement)
	}
	if f.DeprecatedIn != "0.23.0" {
		t.Errorf("DeprecatedIn = %q, want 0.23.0", f.DeprecatedIn)
	}
	if got := f.RefusedFrom(); got != "0.25" {
		t.Errorf("RefusedFrom() = %q, want 0.25 (0.23 + %d minors)", got, MinimumMinorReleases)
	}
	if !strings.Contains(f.Migrator, "memqlmigrate") {
		t.Errorf("Migrator = %q; an author needs the command that does the work", f.Migrator)
	}
}

// THE test the mechanism exists for: the same form, the same table, refused at
// a release past the window and not at one inside it. No release has to ship
// for this to be checkable, which is the whole reason the decision takes a
// release rather than reading a flag somebody has to remember to flip.
func TestRefusesOnlyOnceTheWindowIsSpent(t *testing.T) {
	f, ok := Lookup(ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", ArrayType)
	}
	for _, tc := range []struct {
		release string
		refuses bool
	}{
		{"0.22.8", false}, // before it was even deprecated
		{"0.23.0", false}, // the release that deprecated it
		{"0.24.9", false}, // one minor later, still inside
		{"0.25.0", true},  // the window is spent
		{"0.31.2", true},
		{"1.0.0", true},
		{"", false},    // an unstamped build: unreadable, so it keeps loading
		{"dev", false}, // and so does a dev build
	} {
		if got := f.RefusesAt(tc.release); got != tc.refuses {
			t.Errorf("at release %q, RefusesAt = %v, want %v", tc.release, got, tc.refuses)
		}
	}
}

// Both messages carry every part an author needs: what they wrote, what to
// write, the release that decides it, the rewrite that does the work, and the
// rule a tool keys on. A message missing one of those sends the reader looking.
func TestWarningAndRefusalNameEveryPart(t *testing.T) {
	f, ok := Lookup(ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", ArrayType)
	}
	for _, tc := range []struct {
		name string
		text string
	}{
		{"Warning", f.Warning()},
		{"Refusal", f.Refusal()},
	} {
		for _, want := range []string{f.Spelling, f.Replacement, f.Migrator, f.Rule, f.RefusedFrom(), f.DeprecatedIn} {
			if !strings.Contains(tc.text, want) {
				t.Errorf("%s() does not name %q:\n%s", tc.name, want, tc.text)
			}
		}
	}
	// The warning is window.go's sentence with the rewrite added, so the two
	// cannot drift into saying different things about the same window.
	if !strings.Contains(f.Warning(), New(f.Window("")).Message("`"+f.Spelling+"`", "`"+f.Replacement+"`")) {
		t.Errorf("the warning is not built on the window's own message:\n%s", f.Warning())
	}
}

func TestTrackersAreOnePerFormAndCount(t *testing.T) {
	trackers := Trackers("0.23.0")
	if len(trackers) != len(Forms()) {
		t.Fatalf("%d trackers for %d forms", len(trackers), len(Forms()))
	}
	tr, ok := trackers[ArrayType]
	if !ok {
		t.Fatalf("no tracker for %s", ArrayType)
	}
	tr.Record(ArrayType)
	tr.Record(ArrayType)
	if got := tr.Counts()[ArrayType]; got != 2 {
		t.Errorf("counted %d uses, want 2", got)
	}
	if tr.Refuses(ArrayType) {
		t.Error("a tracker at 0.23.0 refuses a form deprecated in 0.23.0")
	}
	if !Trackers("0.25.0")[ArrayType].Refuses(ArrayType) {
		t.Error("a tracker at 0.25.0 does not refuse a form whose window closed at 0.25")
	}
}

// The process's release is a VALUE, and a test that moves it must be able to
// put it back: the parser, Sense and the load all read it, so one test leaking
// a future release would refuse every later test's source.
func TestSetCurrentRestores(t *testing.T) {
	before := Current()
	restore := SetCurrent("0.25.0")
	if got := Current(); got != "0.25.0" {
		t.Errorf("Current() = %q after SetCurrent, want 0.25.0", got)
	}
	restore()
	if got := Current(); got != before {
		t.Errorf("Current() = %q after restore, want %q", got, before)
	}
}

// An unstamped build -- every test binary, every dev build, the language server
// -- decides at "", which is unreadable, so nothing refuses. That is window.go's
// fail-open posture reaching the registry, and it is what keeps an editor from
// refusing source a cluster would still load.
func TestAnUnstampedBuildRefusesNothing(t *testing.T) {
	restore := SetCurrent("")
	defer restore()
	for _, f := range Forms() {
		if f.RefusesAt(Current()) {
			t.Errorf("form %s refuses in a build that names no release", f.Rule)
		}
	}
}
