package main

import (
	"strings"
	"testing"
)

// The base commit's copies of the two committed files (memql#5389, D21).
//
// Both files live in the tree being judged, so a change can edit either one in
// lockstep with the source and remove the evidence of what it did. -baseline
// and -base-reserved point at the base commit's copies instead; these tests
// cover the ledger half, which is the new behaviour. The surface half is the
// existing Diff, exercised end to end through the CI script by
// scripts/ci/memqlbreaking_base_test.go.

// TestAnErasedReservationIsReported: the entry is gone and the name has NOT
// come back, so the word is now free with nothing recording that it was spent.
func TestAnErasedReservationIsReported(t *testing.T) {
	base := Reservations{Functions: map[string]string{"coalesce": "edition 2026: `a ?? b`."}}
	head := Reservations{Functions: map[string]string{}}
	now := Surface{Functions: map[string]Item{}}

	fs := DiffLedger(base, head, now)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	if fs[0].Category != CategoryMeaning || fs[0].Name != "coalesce" {
		t.Errorf("want meaning/coalesce, got %s/%s", fs[0].Category, fs[0].Name)
	}
	if !strings.Contains(fs[0].Detail, "has NOT come back") {
		t.Errorf("the detail does not distinguish erasure from the return case: %s", fs[0].Detail)
	}
}

// TestAnUnReservedNameComingBackIsSilent is the escape hatch component/language/
// reserved.json's readme documents: a name genuinely returning is un-reserved by
// deleting its entry in the same change that brings it back. Reporting that
// would leave no way to land a return at all, which is closing the hatch rather
// than enforcing the rule around it.
func TestAnUnReservedNameComingBackIsSilent(t *testing.T) {
	base := Reservations{Functions: map[string]string{"coalesce": "edition 2026: `a ?? b`."}}
	head := Reservations{Functions: map[string]string{}}
	now := Surface{Functions: map[string]Item{"coalesce": {Name: "coalesce"}}}

	if fs := DiffLedger(base, head, now); len(fs) != 0 {
		t.Fatalf("want no findings for a name that came back, got %+v", fs)
	}
}

// TestAnUnchangedLedgerIsSilent pins the ordinary case: every PR that does not
// touch the ledger passes this half, so a false positive here reds every build.
func TestAnUnchangedLedgerIsSilent(t *testing.T) {
	ledger := Reservations{
		Annotations: map[string]string{"role": "buried (#2631)."},
		Constructs:  map[string]string{"mutate": "D13."},
		Functions:   map[string]string{"coalesce": "edition 2026."},
	}
	if fs := DiffLedger(ledger, ledger, Surface{}); len(fs) != 0 {
		t.Fatalf("want no findings for an unchanged ledger, got %+v", fs)
	}
}

// TestAddingAReservationIsNotAFinding: the ledger only grows, and growth is the
// expected shape of a deliberate deletion landing.
func TestAddingAReservationIsNotAFinding(t *testing.T) {
	base := Reservations{Functions: map[string]string{}}
	head := Reservations{Functions: map[string]string{"daysBetween": "removed in this change."}}
	if fs := DiffLedger(base, head, Surface{}); len(fs) != 0 {
		t.Fatalf("want no findings for an added reservation, got %+v", fs)
	}
}

// TestTheLedgerSectionsAreComparedSeparately: one word can be an annotation and
// a function at once, and a section-blind comparison would read an erasure in
// one as covered by the other's entry.
func TestTheLedgerSectionsAreComparedSeparately(t *testing.T) {
	base := Reservations{
		Annotations: map[string]string{"role": "buried (#2631)."},
		Functions:   map[string]string{"role": "a different word entirely."},
	}
	head := Reservations{
		Annotations: map[string]string{"role": "buried (#2631)."},
		Functions:   map[string]string{},
	}
	fs := DiffLedger(base, head, Surface{})
	if len(fs) != 1 {
		t.Fatalf("want 1 finding (the function section only), got %d: %+v", len(fs), fs)
	}
	if !strings.Contains(fs[0].Detail, `function "role"`) {
		t.Errorf("the finding does not name the section it came from: %s", fs[0].Detail)
	}
}

// TestABaseSurfaceSeesARemovalTheCommittedBaselineCannot is the property at the
// level of one diff: the same head surface, held to two different bases, gives
// two different answers, and only one of them is the truth.
func TestABaseSurfaceSeesARemovalTheCommittedBaselineCannot(t *testing.T) {
	head := Surface{Edition: "2026", Functions: map[string]Item{"keep": {Name: "keep"}}}
	// The committed baseline as the lockstep change left it: the removed name
	// deleted from here too, so it matches the head exactly.
	edited := Surface{Edition: "2026", Functions: map[string]Item{"keep": {Name: "keep"}}}
	// The base commit's copy, which the change cannot reach.
	base := Surface{Edition: "2026", Functions: map[string]Item{
		"keep": {Name: "keep"}, "removed": {Name: "removed"},
	}}

	if fs := Diff(edited, head, Reservations{}); len(fs) != 0 {
		t.Fatalf("the edited baseline should report nothing -- that is the bypass -- got %+v", fs)
	}
	fs := Diff(base, head, Reservations{})
	if len(fs) != 1 || fs[0].Name != "removed" || fs[0].Category != CategoryParse {
		t.Fatalf("want one parse finding naming `removed`, got %+v", fs)
	}
	if fs := Diff(base, head, Reservations{Functions: map[string]string{"removed": "deliberate."}}); len(fs) != 0 {
		t.Fatalf("a reservation in the HEAD's ledger is what lands a deliberate removal; got %+v", fs)
	}
}
