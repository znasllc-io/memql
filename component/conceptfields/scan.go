// Package conceptfields reads what every concept in the DSL tree DECLARES --
// its top-level field names, which of them are required, and the values of
// every enum -- and holds the committed snapshot of that answer.
//
// ===========================================================================
// WHY A SNAPSHOT EXISTS AT ALL (memql#5209)
// ===========================================================================
// Removing a field from a concept BRICKS every stored row that carries it.
// Every concept builds its JSON schema with `additionalProperties: false` and
// a mutation's read-merge validates the MERGED payload, stored keys included,
// so the next write to such a row is refused with
// `additionalProperties '<field>' not allowed`. memql#5199 wrote the gate for
// the DESTRUCTIVE direction -- a migration that strips too widely -- and could
// not cover this one, because a retirement that shipped no migration leaves
// nothing for a migration-reader to read.
//
// The check that WOULD cover it is "did a field disappear from a concept in
// this branch", and that needs a BEFORE. `git merge-base` cannot supply one:
// every `actions/checkout` in ci.yml is depth-1, so the Go lanes have no
// history at all. A committed snapshot IS the merge base by construction --
// the file in the tree is what main has -- and it works in a depth-1 checkout,
// in `make test`, with no database and no network.
//
// ===========================================================================
// THE SOURCE IS THE BUILT SCHEMA, NOT A SECOND PARSE OF THE TEXT
// ===========================================================================
// Scan runs the loader's own extractor (memql.ExtractConceptDecls, which slices
// concept blocks out and hands each to component/language/parser) and the real
// schema builder, then reads the fields out of the emitted JSON Schema. That is
// deliberate, and it is the whole reason to prefer it over the regex reader
// memql#5199 shipped:
//
//   - The snapshot's claim is exactly "these are the keys a row may carry",
//     and the artifact that decides that at runtime is this same schema. A
//     regex over the source answers a similar question, not the same one.
//   - `@required` is not one spelling. It is the `!` sigil AND the annotation,
//     folded by propertyDeclToParsed. A second implementation of that fold is
//     a second answer to "is this field required", live in a gate whose whole
//     job is to notice when the answer changes.
//   - Enum values live on the TypeRef, and array-of-enum, defaults and
//     variants each shape the emitted schema in ways a line-oriented reader
//     does not see.
//
// ===========================================================================
// TOP-LEVEL FIELDS ONLY, AND SAYING SO IS PART OF THE GATE
// ===========================================================================
// A nested object's members are not top-level payload keys, and `payload - 'x'`
// -- the only strip form a migration writes -- cannot reach one. So the
// snapshot records top-level fields and the ledger governs top-level
// retirements. A narrowing INSIDE a nested block is a real hazard of the same
// family and this file does not cover it; that is written down here rather
// than left for a reader to discover, because a gate that hides what it cannot
// examine is worse than no gate -- an auditor seeing one stops looking.
package conceptfields

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
)

// Entry is one concept's declared surface, as the snapshot records it.
//
// Every slice is sorted and every map is emitted sorted, so the committed file
// is a function of the tree alone -- a generator whose output depended on map
// iteration order would produce a fresh diff on every run and the drift gate
// would be unusable.
type Entry struct {
	Concept string `json:"concept"`
	// File is the source path, carried for the diagnostic rather than for the
	// comparison: a concept that MOVES between files keeps its id, and the
	// snapshot must not read that as a retirement plus an addition. memql#5165
	// missed `active` exactly because a per-file comparison could not see a
	// field move between concepts in one file; keying by concept id is the fix,
	// and File is here so the failure can name where to look.
	File     string              `json:"file"`
	Fields   []string            `json:"fields"`
	Required []string            `json:"required,omitempty"`
	Enums    map[string][]string `json:"enums,omitempty"`
}

// Snapshot is the committed answer: every concept's surface, plus the
// append-only ledger of everything that has been taken away from one.
type Snapshot struct {
	// Concepts is sorted by id.
	Concepts []Entry `json:"concepts"`
	// Retired is the ledger. It is APPEND-ONLY by discipline and by review --
	// see the Retirement type, which says what is and is not enforced about
	// that.
	Retired []Retirement `json:"retired"`
}

// Scan reads every concept declared under dslRoot.
//
// IT WALKS THE TREE THE LOADER'S OWN WAY, and both halves of that matter.
//
//   - dslfs.WalkMemqlFiles is the walk, so the soft-disable convention
//     (`_reference/`, `_`-prefixed files) is honoured by the same code that
//     honours it at boot rather than by a second reading of the same rule.
//   - memql.ExtractConceptDecls is the parse. A `.memql` file is NOT a
//     parseable whole for every construct kind -- handing an automations file
//     to parser.ParseFile fails at the first `automation` keyword -- so the
//     loader slices each concept block out and parses the slice. A snapshot
//     built on the naive parse would silently hold only the files that happen
//     to contain nothing else, which is most of `concepts.memql` and none of
//     the interesting corners.
//
// The whole tree rather than a `concepts.memql` glob, which is what
// memql#5199's reader did: `dsl/shopify/generated/` holds 65 concepts one per
// file, and a glob that misses them leaves the largest single population in
// the tree outside every check built on it. A mirror's rows brick exactly the
// way a native concept's do.
// ScanRoots scans several DSL trees as one snapshot.
//
// The core tree plus every storefront pack's, because a pack in the default
// build holds rows a field drop would brick just as a core concept's does.
// Duplicate-id detection spans the whole set, which is what catches a pack
// shadowing a core concept rather than letting the walk order decide.
func ScanRoots(roots ...string) (Snapshot, error) {
	var all []Entry
	seen := map[string]string{}
	for _, root := range roots {
		snap, err := Scan(root)
		if err != nil {
			return Snapshot{}, err
		}
		for _, e := range snap.Concepts {
			// THE PATH IS MADE ROOT-RELATIVE, because File is the diagnostic
			// that says where to look and two roots both produce
			// "concepts.memql". Only for a non-core root, so every existing
			// entry's File is byte-identical to what it was and the
			// regenerated snapshot's diff is exactly the packs.
			if root != DefaultDSLRoot {
				e.File = path.Join(root, e.File)
			}
			if prior, dup := seen[e.Concept]; dup {
				return Snapshot{}, fmt.Errorf(
					"%s and %s both declare %s: the snapshot cannot record two field sets for one concept",
					prior, e.File, e.Concept)
			}
			seen[e.Concept] = e.File
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Concept < all[j].Concept })
	return Snapshot{Concepts: all}, nil
}

// DefaultRoots is the core tree plus every pack tree on disk, in a stable
// order. A missing packs/ directory simply contributes nothing.
func DefaultRoots() []string {
	roots := []string{DefaultDSLRoot}
	matches, err := filepath.Glob(DefaultPackGlob)
	if err != nil {
		return roots
	}
	sort.Strings(matches)
	return append(roots, matches...)
}

func Scan(dslRoot string) (Snapshot, error) {
	tree := os.DirFS(dslRoot)
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return Snapshot{}, fmt.Errorf("walking %s: %w", dslRoot, err)
	}

	var entries []Entry
	pins := map[string]string{}
	seen := map[string]string{}

	for _, p := range paths {
		raw, readErr := fs.ReadFile(tree, p)
		if readErr != nil {
			return Snapshot{}, fmt.Errorf("%s: %w", p, readErr)
		}
		decls := memql.ExtractConceptDecls(string(raw))
		if len(decls) == 0 {
			continue
		}

		dir := dslfs.NamespaceFromFilePath(p)
		pin, cached := pins[dir]
		if !cached {
			pin = readNamespacePin(dslRoot, dir)
			pins[dir] = pin
		}

		for _, decl := range decls {
			id, idErr := languageAst.AssembleConceptIdFromDeclInDir(decl, dir, pin)
			if idErr != nil || id == "" {
				// The unified loader treats an unassemblable id as a load
				// ERROR, and so does this: a concept whose id cannot be
				// assembled is one whose rows are written under some other id,
				// and guessing here would key the snapshot on a name nothing
				// stores.
				return Snapshot{}, fmt.Errorf("%s: concept %q: %w", p, decl.Name, idErr)
			}
			if prior, dup := seen[id]; dup {
				// Two declarations of one id. memoryNodes.MergeAll would keep
				// one of them and the snapshot would record whichever the walk
				// reached last -- a field set that flips on a filename change.
				return Snapshot{}, fmt.Errorf(
					"%s and %s both declare %s: the snapshot cannot record two field sets for one concept",
					prior, p, id)
			}
			seen[id] = p

			built, buildErr := memoryNodes.BuildConceptFromDecl(decl, id)
			if buildErr != nil {
				return Snapshot{}, fmt.Errorf("%s: concept %s: %w", p, id, buildErr)
			}
			entry, entryErr := entryFromConcept(id, p, built)
			if entryErr != nil {
				return Snapshot{}, fmt.Errorf("%s: concept %s: %w", p, id, entryErr)
			}
			entries = append(entries, entry)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Concept < entries[j].Concept })
	return Snapshot{Concepts: entries}, nil
}

// readNamespacePin reads a domain's `namespace.pin`, the same file the unified
// loader consults. Three exist today (deployment, shopify/overlay,
// shopify/generated) and every one of them changes the assembled id, so a
// snapshot that ignored them would key 65 shopify concepts under the wrong
// namespace and report the whole set as retired the first time anyone looked.
// A pack's pin sits at the ROOT of its own tree (packs/<pack>/dsl/
// namespace.pin) beside root-level construct files, so an empty dir reads
// the root rather than answering "". The core tree has no root-level pin,
// so its behaviour is unchanged -- and it could not have one, since its
// domains are its subdirectories.
func readNamespacePin(dslRoot, dir string) string {
	raw, err := os.ReadFile(filepath.Join(dslRoot, filepath.FromSlash(dir), "namespace.pin"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// entryFromConcept projects the built JSON Schema down to the three things
// that can narrow.
func entryFromConcept(id, file string, c *memoryNodes.Concept) (Entry, error) {
	entry := Entry{Concept: id, File: file}
	raw, ok := c.Schemas[c.SchemaId]
	if !ok {
		// Fall back to the single schema when there is exactly one: SchemaId
		// naming a key the map does not hold would otherwise silently produce
		// a concept with no fields, which reads as "everything was retired".
		if len(c.Schemas) != 1 {
			return Entry{}, fmt.Errorf("schema %q not among the %d built schemas", c.SchemaId, len(c.Schemas))
		}
		for _, only := range c.Schemas {
			raw = only
		}
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return Entry{}, fmt.Errorf("decoding built schema: %w", err)
	}

	entry.Fields = make([]string, 0, len(schema.Properties))
	for name, propRaw := range schema.Properties {
		entry.Fields = append(entry.Fields, name)
		if values := enumValuesOf(propRaw); len(values) > 0 {
			if entry.Enums == nil {
				entry.Enums = map[string][]string{}
			}
			entry.Enums[name] = values
		}
	}
	sort.Strings(entry.Fields)

	entry.Required = append([]string(nil), schema.Required...)
	sort.Strings(entry.Required)
	if len(entry.Required) == 0 {
		entry.Required = nil
	}
	return entry, nil
}

// enumValuesOf reads a property schema's closed value set, at the top level
// and one level into an array's `items`.
//
// THE ARRAY FORM IS NOT OPTIONAL. `[]enum(...)` emits the enum on `items`, and
// a reader that looked only at the top level would record no values for such a
// field -- so removing one would be invisible to the gate, silently, on
// exactly the declaration shape whose stored values are hardest to survey.
func enumValuesOf(propRaw json.RawMessage) []string {
	var prop struct {
		Enum  []any            `json:"enum"`
		Items *json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(propRaw, &prop); err != nil {
		return nil
	}
	values := stringsFromAny(prop.Enum)
	if len(values) == 0 && prop.Items != nil {
		values = enumValuesOf(*prop.Items)
	}
	sort.Strings(values)
	return values
}

func stringsFromAny(in []any) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
