package deprecation

import (
	"strings"
	"testing"
)

// The deprecation window (memql#5390; D22 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// D22: "A public language form deprecates with a load-time warning naming its
// replacement for at least two minor releases before it refuses; deprecated use
// is counted so removal is evidence."
//
// Three claims, and each is tested here because each fails differently:
//
//   - The WARNING must name the replacement. A warning that says only that a
//     form is going away leaves the author to search for what to write instead,
//     which is the half of the message that helps.
//   - The COUNT is what makes a removal evidence rather than a guess. A warning
//     tells one author at one load; a count tells the person deciding whether
//     the form can go whether anybody still writes it.
//   - The REFUSAL is bounded at both ends. Inside the window a deprecated form
//     still loads; after it, it does not. A window that is a constant cannot be
//     tested at both ends, which is why Window carries its values.

func TestMessageNamesTheFormAndItsReplacement(t *testing.T) {
	tr := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: "0.22"})
	got := tr.Message("cond(", "the ternary `p ? a : b`")
	if got == "" {
		t.Fatal("Message returned nothing")
	}
	for _, want := range []string{"cond(", "ternary"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not name %q:\n%s", want, got)
		}
	}
}

// The warning must say WHEN the form stops loading. "This is deprecated" with
// no date is a warning an author can defer forever and then be surprised by.
func TestMessageNamesTheReleaseItRefusesAt(t *testing.T) {
	tr := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: "0.22"})
	got := tr.Message("cond(", "the ternary `p ? a : b`")
	if !strings.Contains(got, "0.24") {
		t.Errorf("the warning does not name the release the form refuses at (0.24 = 0.22 + 2 minors):\n%s", got)
	}
}

func TestUseIsCounted(t *testing.T) {
	tr := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: "0.22"})
	tr.Record("cond(")
	tr.Record("cond(")
	tr.Record("has")
	counts := tr.Counts()
	if got := counts["cond("]; got != 2 {
		t.Errorf("cond( counted %d times, want 2", got)
	}
	if got := counts["has"]; got != 1 {
		t.Errorf("has counted %d times, want 1", got)
	}
	if got := counts["never-written"]; got != 0 {
		t.Errorf("a form nobody wrote counted %d times, want 0", got)
	}
}

// Counts() must hand back a COPY. A caller that ranges over the live map while
// a load is still recording races, and the race detector finds it in CI on
// somebody else's branch.
func TestCountsIsACopy(t *testing.T) {
	tr := New(Window{MinorReleases: 2})
	tr.Record("cond(")
	c := tr.Counts()
	c["cond("] = 99
	c["injected"] = 1
	again := tr.Counts()
	if again["cond("] != 1 {
		t.Errorf("mutating the returned map changed the tracker: cond( is now %d", again["cond("])
	}
	if _, ok := again["injected"]; ok {
		t.Error("a key added to the returned map appeared in the tracker")
	}
}

func TestRefusesOnlyAfterTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		refuses bool
	}{
		{"the release it was deprecated in", "0.22", false},
		{"one minor later, inside the window", "0.23", false},
		{"a patch inside that minor", "0.23.9", false},
		{"two minors later, the window is spent", "0.24", true},
		{"well past the window", "0.31", true},
		{"a whole major later", "1.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: tc.current})
			if got := tr.Refuses("cond("); got != tc.refuses {
				t.Errorf("at %s, Refuses = %v, want %v", tc.current, got, tc.refuses)
			}
		})
	}
}

// An unreadable version is the case that decides whether this mechanism fails
// open or closed, and it has to fail OPEN: a form that refuses because the
// version string could not be parsed is a cluster that will not boot over a
// typo, and the thing being protected is only a warning window.
func TestAnUnreadableVersionDoesNotRefuse(t *testing.T) {
	for _, bad := range []string{"", "dev", "v-unknown", "0.x", "not.a.version"} {
		tr := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: bad})
		if tr.Refuses("cond(") {
			t.Errorf("Current=%q refused: an unreadable version must leave the form loading, "+
				"since a cluster that will not boot over a version typo is worse than a warning that overstays", bad)
		}
	}
	tr := New(Window{MinorReleases: 2, DeprecatedAt: "", Current: "9.9"})
	if tr.Refuses("cond(") {
		t.Error("an empty DeprecatedAt refused: nothing has been deprecated, so nothing can have expired")
	}
}

// A window of zero minor releases would let a form refuse in the release that
// deprecated it, which is the thing D22 exists to prevent. The floor is the
// record's minimum, not the caller's.
func TestWindowIsFlooredAtTheRecordsMinimum(t *testing.T) {
	tr := New(Window{MinorReleases: 0, DeprecatedAt: "0.22", Current: "0.22"})
	if tr.Refuses("cond(") {
		t.Error("a zero-length window let a form refuse in the release that deprecated it")
	}
	if !strings.Contains(tr.Message("cond(", "x"), "0.24") {
		t.Errorf("a zero-length window did not floor to the record's two minors: %s", tr.Message("cond(", "x"))
	}
}

func TestZeroValueTrackerIsUsable(t *testing.T) {
	// A nil tracker is what a caller that never configured one holds. It must
	// not panic: the deprecation path runs inside the loader, and a panic there
	// is a cluster that does not boot.
	var tr *Tracker
	tr.Record("cond(")
	if got := tr.Counts(); len(got) != 0 {
		t.Errorf("a nil tracker counted %v", got)
	}
	if tr.Refuses("cond(") {
		t.Error("a nil tracker refused a form")
	}
	if tr.Message("cond(", "x") == "" {
		t.Error("a nil tracker produced no message; the warning is the one thing it can still do")
	}
}
