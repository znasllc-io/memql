package memql

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestUnifiedLoadersCoverNewTree verifies each of the 5 per-kind
// unified loaders pulls SOME entries from the new tree. This is a
// smoke test, not a coverage assertion -- precise counts will drift
// as the new tree evolves.
func TestUnifiedLoadersCoverNewTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	shapeReg := newShapeRegistry()
	if n, err := LoadUnifiedShapes(logger, shapeReg); err != nil {
		t.Fatalf("LoadUnifiedShapes: %v", err)
	} else {
		t.Logf("shapes: %d", n)
		if n < 40 {
			t.Errorf("shape loader registered %d shapes, expected >= 40 (the dsl tree has ~60). A regression here usually means the parser's must-reference-in-body validator is firing on signature-bound shapes (where the body uses `payload.X` directly and never references `<conceptName>.X`).", n)
		}
	}
	// Spot-check the shapes the identity service magic-link flow
	// reads at runtime (the regression in this class manifested as
	// `shape template "magicLinkRequestFull" not found` post-merge
	// of PR #51).
	for _, name := range []string{"magicLinkRequestFull", "userFull", "delegationFull"} {
		if _, ok := shapeReg.Get(name); !ok {
			t.Errorf("shape registry missing %q -- the parser likely rejected the signature-bound shape with a stale 'must reference in body' validator", name)
		}
	}

	providerReg := newProviderRegistry("")
	if n, err := LoadUnifiedProviders(logger, providerReg); err != nil {
		t.Fatalf("LoadUnifiedProviders: %v", err)
	} else {
		t.Logf("providers: %d", n)
	}

	toolReg := newToolRegistry()
	if n, err := LoadUnifiedTools(logger, toolReg); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	} else {
		t.Logf("tools: %d", n)
	}

	fnReg := newFunctionRegistry()
	if n, err := LoadUnifiedBuiltins(logger, fnReg); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	} else {
		t.Logf("builtins: %d", n)
	}

	// Seeds -- the seed migration replaced `agent X { }` DSL
	// declarations with `seed X { }`. Assert non-zero rather than
	// log-only: a regression in the loader's file-top `use` clause
	// handling silently drops every seed (the role catalog under
	// dsl/agents/roles/ packs many seeds per file sharing one use,
	// and skipping the clause-injection step is exactly what made
	// the cockpit's Agents tab empty in the field). The count is
	// the platform agents (GA + Planner + Trainer) + the role
	// catalog. A precise count would churn -- "at least a dozen"
	// is the floor and the only thing we need to guard against.
	seedReg := NewSeedRegistry()
	n, err := LoadUnifiedSeeds(logger, seedReg)
	if err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	t.Logf("seeds: %d", n)
	if n < 12 {
		t.Errorf("seed loader registered %d seeds, expected >= 12 (3 platform agents + role catalog). "+
			"A regression here usually means the file-top `use <ns>.<concept>` clause is being "+
			"dropped from per-seed slices -- see LoadUnifiedSeeds + fileTopUseClauseRe.", n)
	}

	// At least one of the platform agent seeds must be present so
	// the seed migration's contract (per-user GA / Planner /
	// Trainer rows materialize at startup) doesn't silently break.
	for _, name := range []string{"assistant", "plannerAgent", "trainerAgent"} {
		if _, ok := seedReg.Get(name); !ok {
			t.Errorf("seed registry missing platform seed %q", name)
		}
	}

	// Hyphenated seed names from the role catalog must materialize
	// (memql#180). Before the fix the slicer regex + the seed
	// parser both stopped at `-`, so these silently dropped at
	// load time and the role catalog was missing every kebab-case
	// entry. A regression here would re-empty those rows from the
	// catalog.
	for _, name := range []string{"graphic-designer", "ux-designer", "music-theory-teacher"} {
		if _, ok := seedReg.Get(name); !ok {
			t.Errorf("seed registry missing hyphenated role seed %q -- memql#180 regression?", name)
		}
	}

	// Force-reference toolReg so the legacy import that used to
	// hand it to LoadUnifiedAgents doesn't become unused.
	_ = toolReg

	// Function-shaped constructs (mutations / queries / logic /
	// automation) go through LoadUnifiedFunctions. After the
	// import-model migration (PR #48), every query / mutation / shape
	// declaration carries its concept binding in the SIGNATURE
	// (`mutation <Concept> <name> { ... }`); the slice extractor's
	// header regex must tolerate that optional second identifier or
	// it strips the wrong token as the function name and the
	// FunctionRegistry ends up with the concept in the slot where
	// the function name should live. The runtime then can't resolve
	// any mutation/query call -- "function not found" at every dev
	// startup. This block guards against that regression.
	fnReg2 := newFunctionRegistry()
	total, counts, err := LoadUnifiedFunctions(logger, fnReg2, nil)
	if err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	t.Logf("functions: total=%d counts=%v", total, counts)
	for _, name := range []string{
		"setGlobalSecret",
		"createAgentRole",
		"updateNodeHealth",
		"activeUsers",
		"allAgents",
	} {
		if fn, err := fnReg2.Get(name); err != nil || fn == nil {
			t.Errorf("function registry missing %q (err=%v) -- the slice extractor probably dropped the function name (signature-bound concept regex regression)", name, err)
		}
	}
	// nil-registry path skips concept resolution; the live engine runs
	// LoadUnifiedFunctions with the populated default registry and
	// that's the surface that actually has to be green at dev-refresh
	// time. LoadUnifiedConcepts populates the global registry; we
	// pass DefaultRegistry() into LoadUnifiedFunctions here so the
	// signature-bound concept resolution branch fires.
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := memoryNodes.DefaultRegistry()
	fnReg3 := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, fnReg3, conceptRegistry); err != nil {
		t.Fatalf("LoadUnifiedFunctions (with concept registry): %v", err)
	}
	// Spot-check a mutation that depends on signature-bound concept
	// resolution -- without it the function loads but the runtime
	// errors with `concept "globalSecret" not found`. Check three
	// surfaces:
	//
	//   - fn.BoundConcept -- the implicit binding the function loader
	//     stamps onto the Function for downstream consumers.
	//   - fn.MutationTemplate.Concept -- the canonical id the engine
	//     executes against. MUST be the canonical id (contains ":");
	//     bare names error at executeInsert time.
	//
	// These two are populated by different code paths (BoundConcept
	// fallback vs concept-resolver walking the FunctionDef); both
	// must be green or the runtime breaks in different ways.
	if fn, err := fnReg3.Get("setGlobalSecret"); err != nil || fn == nil {
		t.Errorf("setGlobalSecret missing after full load: err=%v", err)
	} else {
		if fn.BoundConcept == "" {
			t.Errorf("setGlobalSecret.BoundConcept empty -- signature-bound concept resolution regressed")
		} else if !strings.Contains(fn.BoundConcept, "globalSecret") {
			t.Errorf("setGlobalSecret.BoundConcept = %q, expected to contain 'globalSecret'", fn.BoundConcept)
		}
		if fn.MutationTemplate == nil {
			t.Errorf("setGlobalSecret.MutationTemplate is nil")
		} else if !strings.Contains(fn.MutationTemplate.Concept, ":") {
			t.Errorf("setGlobalSecret.MutationTemplate.Concept = %q -- expected canonical id (containing ':'), the resolver didn't walk the FunctionDef", fn.MutationTemplate.Concept)
		}
	}
}
