package conceptfields

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// DefaultSnapshotPath is where the committed snapshot lives, relative to the
// repository root.
const DefaultSnapshotPath = "component/conceptfields/concept-fields.snapshot.json"

// DefaultDSLRoot and DefaultMigrationsDir are the two trees the snapshot is
// derived from and checked against.
const (
	DefaultDSLRoot       = "dsl"
	DefaultMigrationsDir = "component/database/memory-nodes/migrations"

	// DefaultPackGlob finds every STOREFRONT PACK's DSL tree (epic
	// memql#5532, issue memql#5549).
	//
	// PACKS ARE SNAPSHOTTED BECAUSE THEY SHIP NOW. While a pack lived under
	// examples/ behind a build tag no published image set, its concepts
	// could hold no rows on any cluster and there was nothing for this gate
	// to protect. Linking the storefront packs into the default build makes
	// their rows as real as any core concept's, so dropping a field from
	// v1:reviews:review would brick stored rows exactly as dropping one
	// from a core concept would -- silently, because CI's db-tests run on a
	// fresh database.
	//
	// A GLOB RATHER THAN A LIST, so the wholesale pack and whatever follows
	// it are covered the day they land rather than the day somebody
	// remembers this file.
	DefaultPackGlob = "packs/*/dsl"
)

// readme is emitted into the file itself.
//
// UNUSUAL FOR A GENERATED FILE, AND ON PURPOSE HERE: every other regeneration
// gate in this repo writes an artifact nobody edits by hand, so a header would
// only be read by someone already reading the generator. This one has a
// HAND-AUTHORED half -- the retirement ledger -- and the person editing it is
// mid-way through a failing build, looking at the file rather than at a
// package doc. The lines below are what they need in front of them.
var readme = []string{
	"GENERATED plus HAND-AUTHORED. Run `make concept-snapshot` to refresh `concepts`.",
	"`concepts` is every concept's top-level field surface, taken from the built JSON schema.",
	"`retired` is the append-only ledger: what has been taken away from a concept, and what was done about it.",
	"Removing a field from a concept BRICKS its stored rows -- additionalProperties is false and the read-merge validates the merged payload -- so a retirement needs a migration, or a waiver saying why it does not.",
	"The generator REFUSES to drop a field from `concepts` until a `retired` entry covers it. That refusal is the gate (memql#5209).",
}

// File is the on-disk shape.
type File struct {
	Readme   []string     `json:"readme"`
	Concepts []Entry      `json:"concepts"`
	Retired  []Retirement `json:"retired"`
}

// Load reads the committed snapshot. A missing file is an EMPTY snapshot and
// no error, so the very first generation has a `before` to diff against --
// which is nothing, and therefore reports no narrowing. Every field in the
// tree on the day this lands is baseline, not a retirement.
func Load(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	return Snapshot{Concepts: f.Concepts, Retired: f.Retired}, nil
}

// Marshal renders a snapshot to the exact bytes the file should hold.
//
// One rendering, used by both the writer and the drift check, so `--check` can
// never disagree with what a write would have produced -- the failure mode
// where a gate reports drift that regenerating does not fix.
func Marshal(s Snapshot) ([]byte, error) {
	f := File{Readme: readme, Concepts: s.Concepts, Retired: s.Retired}
	if f.Concepts == nil {
		f.Concepts = []Entry{}
	}
	if f.Retired == nil {
		f.Retired = []Retirement{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
