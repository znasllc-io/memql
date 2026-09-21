package main

// The reservation ledger (memql#5389, D21).
//
// # What a reservation is for
//
// It is what makes a deletion SAFE rather than merely recorded. Deleting a name
// from the authoring surface breaks the files that write it, and memqlbreaking
// says so. But the deletion leaves a second hole that no diff of two surfaces
// can see on its own: the name is now FREE. Give it to something else in a
// later release and a bundle still carrying the old spelling loads under the
// new meaning and does the new thing, silently, with no refusal anywhere.
//
// So the ledger does two jobs, and both are enforced in diff.go:
//
//   - a deletion WITH an entry is accepted and produces no finding;
//   - a name WITH an entry that reappears in the new surface is a finding,
//     even though it arrived rather than left.
//
// The second is the one that costs something, and it is the one that makes the
// first mean anything. Without it the ledger is a receipt.
//
// # Append-only, and how to un-reserve
//
// Un-reserving is the escape hatch, stated so the next reader does not have to
// decide whether one exists: if a reserved name really is the same thing coming
// back, delete its entry in the same change that brings it back, and the
// reviewer reads one line instead of nothing.
//
// Everything else here only grows, and with a BASE LEDGER (-base-reserved) that
// is a mechanism rather than a rule: DiffLedger reports an entry the base commit
// held that this tree has dropped while the name did NOT come back. The hatch is
// exactly the carve-out -- a name that is back on the surface is the sentence
// above, and produces nothing. What is left is pure erasure: the name stays gone
// AND the record that it was ever spent goes with it, which is the same
// edit-both-sides move -base-reserved's sibling flag exists to close. It lands
// on the MEANING axis because the hazard a reservation exists to prevent is a
// spent word coming back meaning something else, and erasing the entry is that
// break's enabling move rather than a break of its own.
//
// # Why there is no shapes section
//
// The ledger reserves WORDS -- a construct keyword, an annotation name, a
// function name -- because a word can be handed to something else. A shape is
// DSL-tree content rather than a word of the language: a removed shape is
// reported as a wire break and acknowledged by re-capturing the baseline in the
// same change, which is itself a committed diff a reviewer reads. Reserving a
// shape name would suggest the engine could one day mean something else by it,
// which it cannot.
//
// # Its precedent
//
// component/conceptfields/concept-fields.snapshot.json holds the same rule for
// concept FIELDS and predates this file: a generated section, a hand-authored
// `retired` ledger, a `readme` array saying which is which, and a generator
// that refuses to drop an entry the ledger does not cover. This file is that
// shape applied to the language surface.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReservedPath is where the ledger is committed, relative to the repo root.
const ReservedPath = "component/language/reserved.json"

// BaselinePath is where the committed surface baseline lives, relative to the
// repo root. It is the previous capture the CI check diffs against.
const BaselinePath = "component/language/surface/2026.json"

// Reservations is the ledger. Each section maps a spent name to the note that
// says what happened to it; a blank note is refused by the ledger's own test,
// because a reservation that records only that a name is gone leaves out the
// half a reader needs.
type Reservations struct {
	Readme      []string          `json:"readme"`
	Annotations map[string]string `json:"annotations"`
	Constructs  map[string]string `json:"constructs"`
	Functions   map[string]string `json:"functions"`
}

// LoadReservations reads the ledger. A missing file is an ERROR rather than an
// empty ledger: an empty ledger accepts no deletion at all, so a typo'd path
// would turn every already-retired name into an unreserved break and the fix a
// reader reaches for is to add the entries again.
func LoadReservations(path string) (Reservations, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Reservations{}, fmt.Errorf("reading the reservation ledger: %w", err)
	}
	var r Reservations
	if err := json.Unmarshal(raw, &r); err != nil {
		return Reservations{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	if r.Annotations == nil {
		r.Annotations = map[string]string{}
	}
	if r.Constructs == nil {
		r.Constructs = map[string]string{}
	}
	if r.Functions == nil {
		r.Functions = map[string]string{}
	}
	return r, nil
}

// DiffLedger holds this tree's ledger to the one the BASE COMMIT carried, and
// reports every reservation that was erased rather than spent.
//
// It is the other half of holding a change to the base commit. The surface
// baseline answers "was this name on the surface before?"; the ledger answers
// "was this name already spent?". Both questions are asked of files the change
// itself may edit, so both have to be read from the side the change cannot
// rewrite -- which is what -base-reserved supplies.
//
// The one carve-out is the documented hatch: a name that is BACK on the head's
// surface is a name genuinely returning, and deleting its entry in that same
// change is how the ledger says so. This function is silent there, and so is
// the resurrection check, which is the hatch working as reserved.json's readme
// describes it.
//
// base and head are compared section by section, because a name may legitimately
// be an annotation and a function at once.
func DiffLedger(base, head Reservations, now Surface) []Finding {
	var fs []Finding
	sections := []struct {
		kind       string
		base, head map[string]string
		surface    map[string]Item
	}{
		{"annotation", base.Annotations, head.Annotations, now.Annotations},
		{"construct", base.Constructs, head.Constructs, now.Constructs},
		{"function", base.Functions, head.Functions, now.Functions},
	}
	for _, s := range sections {
		for _, name := range sortedKeys(s.base) {
			if _, stillReserved := s.head[name]; stillReserved {
				continue
			}
			if _, cameBack := s.surface[name]; cameBack {
				continue // the hatch: the same thing genuinely returning
			}
			fs = append(fs, Finding{
				Category: CategoryMeaning,
				Name:     name,
				Was:      s.base[name],
				Detail: fmt.Sprintf("the %s %q was reserved in %s at the base commit and this change drops that "+
					"entry, while the name has NOT come back on the surface. The reservation is the only record "+
					"that the word is spent; without it the name is free to be given to something else later, and "+
					"a bundle still carrying the old spelling would load under the new meaning and do the new "+
					"thing silently. Put the entry back. Deleting one is how a name genuinely RETURNS -- in the "+
					"same change that brings it back -- and that case is reported by neither this line nor the "+
					"resurrection check.", s.kind, name, ReservedPath),
			})
		}
	}
	return fs
}

// LoadSurface reads a captured surface. Nil maps are normalised to empty ones,
// so a hand-trimmed fixture and a full capture diff by the same rules.
func LoadSurface(path string) (Surface, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Surface{}, fmt.Errorf("reading the baseline surface: %w", err)
	}
	var s Surface
	if err := json.Unmarshal(raw, &s); err != nil {
		return Surface{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	if s.Constructs == nil {
		s.Constructs = map[string]Item{}
	}
	if s.Annotations == nil {
		s.Annotations = map[string]Item{}
	}
	if s.Functions == nil {
		s.Functions = map[string]Item{}
	}
	if s.Shapes == nil {
		s.Shapes = map[string][]string{}
	}
	return s, nil
}

// WriteSurface commits a capture. It is indented and newline-terminated
// because the file is read in review far more often than by this command, and
// a one-line JSON blob makes a surface change unreviewable.
func WriteSurface(path string, s Surface) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the surface: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// defaultReservedPath and defaultBaselinePath resolve the two committed files
// against the REPO ROOT rather than the working directory, so `make
// memqlbreaking` from the root and `go test ./cmd/memqlbreaking/` from the
// package directory read the same files. Without it the tests would need a
// relative path of their own, which is a second answer to "where is the
// ledger" and the kind that drifts.
func defaultReservedPath() string { return repoFile(ReservedPath) }

func defaultBaselinePath() string { return repoFile(BaselinePath) }

// repoFile joins rel onto the repo root, found by walking up from the working
// directory to the first directory holding go.work -- the one file that exists
// exactly once, at the top of this repo. If none is found the relative path is
// returned unchanged, so a caller outside a checkout gets a plain "no such
// file" naming what it looked for rather than an absolute path to nowhere.
func repoFile(rel string) string {
	dir, err := os.Getwd()
	if err != nil {
		return rel
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return rel
		}
		dir = parent
	}
}
