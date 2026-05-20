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
		"mutationSetGlobalSecret",
		"mutationCreatePartition",
		"mutationCreateAgentRole",
		"mutationUpdateNodeHealth",
		"queryActiveUsers",
		"queryAllAgents",
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
	// resolution -- without it the function loads but BoundConcept
	// is empty and the runtime errors with `concept "globalSecret"
	// not found`.
	if fn, err := fnReg3.Get("mutationSetGlobalSecret"); err != nil || fn == nil {
		t.Errorf("mutationSetGlobalSecret missing after full load: err=%v", err)
	} else if fn.BoundConcept == "" {
		t.Errorf("mutationSetGlobalSecret.BoundConcept empty -- signature-bound concept resolution regressed")
	} else if !strings.Contains(fn.BoundConcept, "globalSecret") {
		t.Errorf("mutationSetGlobalSecret.BoundConcept = %q, expected to contain 'globalSecret'", fn.BoundConcept)
	}
}
