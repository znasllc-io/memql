package memql

// construct_slices_v1_test.go -- slicing an edition-2026 tree (epic
// memql#5363, task memql#5368).
//
// A brace-less spec or trait (`spec agent isX = row => ...`) has no brace for
// a header regexp to anchor on, so every slicer that required one lost every
// spec and trait of a migrated tree in silence: the loader, the duplicate
// detector, the construct catalog and the authoring bundle splitter slice
// through constructDeclarationSlices, and the language server locates the same
// declarations by walking tokens. These pin both halves -- that the
// declarations are FOUND, and that the two sides cut them at the same bytes,
// the parity the construct source hash rests on (memql#3758).

import (
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/sense"
)

const braceLessFixture = `use agents.concepts.{ agent }

/// Assistants only.
@enabled
spec agent isAssistant = row => row.role == "assistant"
                         && row.kind != "system"   // not the system agent

/// Rows marked active.
trait isActiveRecord = row => row.active == true

/// A braced spec beside them.
spec agent isLegacy = row => row.role == "x"

/// Reads active assistants.
@unbounded("fixture")
query agent activeAssistants {
  filter  row => isAssistant(row) && isActiveRecord(row)
  shape   agentFull
}
`

func TestKeywordSlicesFindBraceLessPredicates(t *testing.T) {
	var specs, traits []string
	for _, s := range ExtractKeywordSlices(braceLessFixture, "spec") {
		specs = append(specs, s.Name)
	}
	for _, s := range ExtractKeywordSlices(braceLessFixture, "trait") {
		traits = append(traits, s.Name)
	}
	if strings.Join(specs, ",") != "isAssistant,isLegacy" || strings.Join(traits, ",") != "isActiveRecord" {
		t.Fatalf("spec slices = %v, trait slices = %v; want [isAssistant isLegacy] and [isActiveRecord], in source order", specs, traits)
	}
}

// constructHashParityOf compares the engine's slices of one file with the
// language server's, per (kind, name), and returns the mismatches plus how many
// spec and trait declarations were compared.
func constructHashParityOf(content string) (mismatches []string, predicates int) {
	engine := map[string]string{}
	forEachConstructSource(content, func(kind, name, source string) {
		key := kind + " " + name
		if _, seen := engine[key]; !seen {
			engine[key] = sense.ConstructSourceHash(source)
		}
	})
	lsp := map[string]string{}
	for _, c := range sense.ConstructHashes(content) {
		kind, known := ConstructKindForKeyword(c.Kind)
		if !known {
			continue
		}
		key := kind + " " + c.Name
		if _, seen := lsp[key]; !seen {
			lsp[key] = c.SourceHash
		}
		if c.Kind == "spec" || c.Kind == "trait" {
			predicates++
		}
	}
	for key, h := range engine {
		if lsp[key] != h {
			mismatches = append(mismatches, key)
		}
	}
	for key := range lsp {
		if _, ok := engine[key]; !ok {
			mismatches = append(mismatches, key+" (language server only)")
		}
	}
	sort.Strings(mismatches)
	return mismatches, predicates
}

func TestConstructHashParityOnBraceLessPredicates(t *testing.T) {
	mismatches, predicates := constructHashParityOf(braceLessFixture)
	if predicates != 3 {
		t.Fatalf("the language server located %d spec/trait declarations, want 3", predicates)
	}
	if len(mismatches) != 0 {
		t.Fatalf("the engine and the language server cut these differently: %v", mismatches)
	}
}

// TestConstructHashParityOnTheMigratedCorpus is the corpus parity gate over
// the embedded tree: every construct, in every file, cut at the same bytes by
// both sides. Before the flip the tree was migrated in memory with the
// expressions codemod first; since the flip the tree IS edition 2026, so the
// gate reads its files as they are, and the codemod has nothing left to
// rewrite in them.
func TestConstructHashParityOnTheMigratedCorpus(t *testing.T) {
	files := baseloader.ReadAll(nil)
	predicates := 0
	for _, f := range files {
		mismatches, n := constructHashParityOf(f.Content)
		predicates += n
		for _, m := range mismatches {
			t.Errorf("%s: %s: the engine and the language server cut this construct differently", f.Path, m)
		}
	}
	// Measured when the floor was set: 39 specs and traits over 126 migrated
	// files; after the flip, 39 over the tree's 308 files. Without the
	// brace-less arms both sides see zero of them and agree -- which is why
	// the count, not the agreement, is the floor.
	if predicates < 30 || len(files) < 100 {
		t.Errorf("compared %d spec/trait declarations over %d files -- the scan has stopped reaching them", predicates, len(files))
	}
	t.Logf("%d spec/trait declarations cut identically by both sides over %d files", predicates, len(files))
}
