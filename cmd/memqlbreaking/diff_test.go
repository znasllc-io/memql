package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// memqlbreaking classifies a change on three axes (memql#5389, D21): parse (a
// form stops parsing), meaning (the same text does something different) and
// wire (a generated SDK method, a shape key or an event payload changes name).
//
// The command exists because a breaking change reaches a bundle author as a
// load failure in their tree, weeks later, with no way to tell a deliberate
// retirement from an accident. A classified, named finding at the commit that
// causes it is the whole product.

func TestDeletedAnnotationWithoutReservationIsParse(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"serverOnly": {Name: "serverOnly"}}}
	now := Surface{Annotations: map[string]Item{}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	if fs[0].Category != CategoryParse {
		t.Errorf("a deleted annotation is a parse break, got %q", fs[0].Category)
	}
	if fs[0].Name != "serverOnly" {
		t.Errorf("the finding does not name what was deleted: %+v", fs[0])
	}
}

// The reservation is what makes a deletion SAFE rather than merely recorded: a
// reserved name can never come back with another meaning, which is the failure
// a bare deletion leaves open -- a bundle written against the old meaning loads
// under the new one and does something else.
func TestDeletedAnnotationWithReservationIsAccepted(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"rateLimit": {Name: "rateLimit"}}}
	now := Surface{Annotations: map[string]Item{}}
	res := Reservations{Annotations: map[string]string{"rateLimit": "memql#5375: read by nothing; the tool's limit was never enforced"}}
	if fs := Diff(old, now, res); len(fs) != 0 {
		t.Errorf("a reserved deletion is not a finding, got %+v", fs)
	}
}

func TestShapeKeyRenameIsWire(t *testing.T) {
	old := Surface{Shapes: map[string][]string{"folderCard": {"id", "name", "createdAt"}}}
	now := Surface{Shapes: map[string][]string{"folderCard": {"id", "title", "createdAt"}}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryWire {
		t.Fatalf("a shape key rename is a wire break, got %+v", fs)
	}
	if fs[0].Was != "name" || fs[0].Now != "title" {
		t.Errorf("the finding does not name both sides of the rename: %+v", fs[0])
	}
}

func TestAnnotationArgFormChangeIsMeaning(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"cache": {Name: "cache", Form: "ttl=<number>"}}}
	now := Surface{Annotations: map[string]Item{"cache": {Name: "cache", Form: "<number>"}}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryMeaning {
		t.Fatalf("an argument-form change is a meaning break, got %+v", fs)
	}
}

// An ADDITION is never a finding. memqlbreaking reports BREAKS; a command that
// reports every change is a command whose output nobody reads, and the one
// signal that matters drowns.
func TestAdditionIsNotAFinding(t *testing.T) {
	old := Surface{Annotations: map[string]Item{}}
	now := Surface{Annotations: map[string]Item{"loop": {Name: "loop"}}}
	if fs := Diff(old, now, Reservations{}); len(fs) != 0 {
		t.Errorf("an added annotation is not a break, got %+v", fs)
	}
}

func TestEditionChangeIsReported(t *testing.T) {
	old := Surface{Edition: "2025"}
	now := Surface{Edition: "2026"}
	fs := Diff(old, now, Reservations{})
	if len(fs) == 0 {
		t.Fatal("an edition change is reported: it is the one change that reclassifies every other finding")
	}
}

// A RESERVED name coming back is the failure the ledger exists to prevent, and
// it is the half of the contract a bare receipt does not hold. Reserving
// `rateLimit` records that files may still carry it; a later release that gives
// the name back to something else loads those files and does the new thing
// silently. So a reserved name present in the NEW surface is a finding, even
// though it is an addition -- it is a resurrection, not an addition.
func TestReservedNameComingBackIsAFinding(t *testing.T) {
	old := Surface{Annotations: map[string]Item{}}
	now := Surface{Annotations: map[string]Item{"rateLimit": {Name: "rateLimit"}}}
	res := Reservations{Annotations: map[string]string{"rateLimit": "memql#5375: the declared ceiling did not exist"}}
	fs := Diff(old, now, res)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding for a reserved name returning, got %d: %+v", len(fs), fs)
	}
	if fs[0].Name != "rateLimit" || fs[0].Category != CategoryMeaning {
		t.Errorf("a reserved name returning is a meaning break naming it, got %+v", fs[0])
	}
}

// A receiver leaving an annotation's accepted set is a parse break even though
// the annotation itself survives: `@disabled` on a provider stopping being
// accepted refuses every provider file that writes it, and the finding has to
// say WHERE rather than merely that the name still exists.
func TestReceiverRemovedFromAnAnnotationIsParse(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"disabled": {Name: "disabled", On: []string{"provider", "query"}}}}
	now := Surface{Annotations: map[string]Item{"disabled": {Name: "disabled", On: []string{"query"}}}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryParse {
		t.Fatalf("a removed receiver is a parse break, got %+v", fs)
	}
	if !strings.Contains(fs[0].Detail, "provider") {
		t.Errorf("the finding does not name the receiver that was removed: %+v", fs[0])
	}
}

// A whole shape disappearing is a wire break: every client reading its keys
// reads nothing. Reported once, naming the shape, rather than once per key --
// a finding per key of a deleted shape buries the one fact that matters.
func TestDeletedShapeIsOneWireFinding(t *testing.T) {
	old := Surface{Shapes: map[string][]string{"folderCard": {"id", "name"}}}
	now := Surface{Shapes: map[string][]string{}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryWire || fs[0].Name != "folderCard" {
		t.Fatalf("a deleted shape is one wire finding naming it, got %+v", fs)
	}
}

// TestSelfTestOverABreakingFixture is the negative control: the committed
// baseline mutated in four ways, one per category plus one reserved deletion,
// run through the real Diff. A gate nobody has seen fire is a gate nobody has
// tested.
func TestSelfTestOverABreakingFixture(t *testing.T) {
	base := readSurface(t, "testdata/baseline.json")
	broken := readSurface(t, "testdata/breaking.json")
	fs := Diff(base, broken, readReservations(t, "testdata/reserved.json"))
	got := map[Category]int{}
	for _, f := range fs {
		got[f.Category]++
	}
	for _, want := range []Category{CategoryParse, CategoryMeaning, CategoryWire} {
		if got[want] == 0 {
			t.Errorf("the breaking fixture produced no %s finding; the fixture or the classifier has stopped covering that axis", want)
		}
	}
	// The reserved deletion must NOT appear. Without this the fixture proves
	// only that the classifier reports things, not that the ledger is read.
	for _, f := range fs {
		if f.Name == "scopes" {
			t.Errorf("the reserved deletion of %q was reported: the ledger is not being read. %+v", f.Name, f)
		}
	}
	t.Logf("self-test over the breaking fixture: %d findings (%d parse, %d meaning, %d wire)",
		len(fs), got[CategoryParse], got[CategoryMeaning], got[CategoryWire])
}

// readSurface decodes a committed fixture. It is deliberately the same decoder
// the command uses for -baseline: a fixture read through a second decoder is a
// fixture that proves nothing about the first.
func readSurface(t *testing.T, path string) Surface {
	t.Helper()
	s, err := LoadSurface(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return s
}

func readReservations(t *testing.T, path string) Reservations {
	t.Helper()
	r, err := LoadReservations(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return r
}

// The ledger cannot fall behind the retirement tables. Every annotation the
// parser has retired EVERYWHERE and that no receiver still accepts is a name
// files in the wild still carry, so it is exactly the class the ledger exists
// for: reserve it, or the next capture of the surface silently forgets it was
// ever a word.
func TestEveryRetiredAnnotationIsReserved(t *testing.T) {
	res, err := LoadReservations(defaultReservedPath())
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	live := Capture().Annotations
	var missing []string
	for _, name := range retiredEverywhereNames() {
		if _, stillLive := live[name]; stillLive {
			continue
		}
		if _, reserved := res.Annotations[name]; !reserved {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d retired annotation(s) carry no reservation in %s:\n  %s\n\n"+
			"A retired name that is not reserved can be handed to something else in a later release, "+
			"and a bundle still carrying the old spelling would load under the new meaning and do the "+
			"new thing silently. Add an entry naming the issue and what happened to it.",
			len(missing), defaultReservedPath(), strings.Join(missing, "\n  "))
	}
}

// The ledger's own shape is part of its contract: an entry whose note is blank
// records that a name is spent and nothing about why, which is the half a
// reader needs. The concept-field ledger holds the same rule for its `note`.
func TestEveryReservationCarriesANote(t *testing.T) {
	raw, err := os.ReadFile(defaultReservedPath())
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	var doc struct {
		Readme      []string          `json:"readme"`
		Annotations map[string]string `json:"annotations"`
		Constructs  map[string]string `json:"constructs"`
		Functions   map[string]string `json:"functions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the ledger: %v", err)
	}
	if len(doc.Readme) == 0 {
		t.Error("the ledger carries no readme: the next reader has no way to know it is append-only")
	}
	total := 0
	for section, entries := range map[string]map[string]string{
		"annotations": doc.Annotations, "constructs": doc.Constructs, "functions": doc.Functions,
	} {
		for name, note := range entries {
			total++
			if strings.TrimSpace(note) == "" {
				t.Errorf("%s.%s reserves the name and says nothing about why", section, name)
			}
		}
	}
	if total == 0 {
		t.Error("the ledger is empty: every already-retired name belongs in it, or the first real run " +
			"reports the whole of the retirement epics as unreserved breaks")
	}
}
