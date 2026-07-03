package memql

// deploy_pack_load_test.go is the in-package half of the Epic 2 / #2095 deploy
// pack verification. It loads the pack's REAL dsl/builtins.memql through the
// unified builtin loader and asserts all four deploy effect builtins register --
// proving the @executor("integration.deploypack.<cap>") wiring parses.
//
// Why in-package (package memql): LoadUnifiedBuiltins needs a *FunctionRegistry
// built by the unexported newFunctionRegistry constructor, and we cannot import
// examples/deploypack from a component/memql test without an import cycle
// (deploypack imports component/memql). So this test mounts the pack's real
// builtins.memql file (read from disk) as an overlay and exercises the actual
// pack artifact through the loader. The companion
// examples/deploypack/deploy_pack_test.go proves the provider-capability +
// handler-effect + contract halves via exported API.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestDeployPackBuiltinsLoad loads the deploy pack's real builtins.memql and
// asserts every deploy effect builtin registers, so each
// @executor("integration.deploypack.<cap>") parses and is resolvable.
func TestDeployPackBuiltinsLoad(t *testing.T) {
	builtinPath := filepath.Join("..", "..", "examples", "deploypack", "dsl", "builtins.memql")
	builtinSrc, err := os.ReadFile(builtinPath)
	if err != nil {
		t.Fatalf("reading deploy pack builtins.memql: %v", err)
	}
	overlay := fstest.MapFS{"builtins.memql": {Data: builtinSrc}}
	const domain = "deploypackbuiltinload"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins failed: %v", err)
	}

	for _, name := range []string{
		"deployCommitOverlay",
		"deployArgoSync",
		"deployRunPromote",
		"deployRecordBack",
		"deployObserveReconciledState", // E2.4 Model A read leg
	} {
		if _, err := registry.Get(name); err != nil {
			t.Errorf("deploy pack builtin %q MUST register (its @executor wiring must parse): %v", name, err)
		}
	}
}

// TestDeployPackLifecycleAutomationLoads is the E2.3 (#2096) load proof: mount
// the deploy pack's ENTIRE dsl/ tree (builtins + the lifecycle automation +
// its logic) under the deploypack domain and run the SAME full engine Init the
// cluster runs at boot. It verifies:
//   - the pack's effect builtins (deployRunPromote, ...) register via their
//     @executor wiring, AND
//   - the CORE cross-namespace mutation `updateDeploymentStatus` the port
//     transitions through resolves.
//
// STRICT-BOOT INTERACTION (epic #2351 / S2, memql#2357): strict boot revealed
// that the pack's two lifecycle LOGIC constructs (driveDeploymentInProgress /
// recordReconciledState) do NOT parse under the current logic-step grammar --
// their bodies use bare scalar (`status := coalesce(...)`) and bare boolean
// (`isAzure := provider == ...`) `:=` steps, a form the step parser rejects
// (each `:=` step RHS must be a function call, an `if cond { call }`, or a
// collection chain). Those two constructs were ALWAYS silently skipped at load;
// this test previously passed green because it only ever asserted on the
// builtins + the core mutation, never on the logic -- the exact silent-drop /
// false-green class epic #2351 exists to kill. Strict boot now surfaces the
// drop loudly. Fixing the deploy-pack logic's authoring is a follow-up outside
// S2's scope (it's an example-pack grammar-fit fix, not a LoadReport change);
// until then this test boots via the MEMQL_DSL_ALLOW_SKIPS break-glass and
// ASSERTS that the load report names both skipped logic constructs -- a
// positive demonstration of exactly the break-glass + report S2 introduces.
//
// Mounted from disk (not via a Go import of examples/deploypack, which would be
// an import cycle) so it exercises the real pack artifacts.
func TestDeployPackLifecycleAutomationLoads(t *testing.T) {
	// Break-glass: the pack's lifecycle logic has a pre-existing parse gap
	// (see the doc comment) that strict boot now surfaces. Boot anyway so we
	// can still verify the builtins + core-mutation import resolve, and assert
	// the report captured the skips.
	t.Setenv(allowSkipsEnvVar, "1")

	packDSL := filepath.Join("..", "..", "examples", "deploypack", "dsl")
	entries, err := os.ReadDir(packDSL)
	if err != nil {
		t.Fatalf("reading pack dsl dir: %v", err)
	}
	overlay := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(packDSL, e.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", e.Name(), readErr)
		}
		overlay[e.Name()] = &fstest.MapFile{Data: data}
	}
	const domain = "deploypack"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	// Load the unified concept tree (core + nothing pack-specific; the pack
	// adds no concepts) and run the full engine Init over the mounted tree.
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after load")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the core tree + mounted deploy pack failed "+
			"(with the break-glass set, so this is NOT the strict-boot refusal): %v", err)
	}

	// The pack's effect builtins the lifecycle logic fires must be registered
	// as functions after Init, and so must the core cross-namespace mutation.
	fns := eng.Functions()
	for _, name := range []string{
		"deployRunPromote",       // the azure effect the lifecycle logic fires
		"updateDeploymentStatus", // the core mutation it transitions through
	} {
		if !fns.Has(name) {
			t.Errorf("expected %q to be registered after Init over the mounted deploy pack", name)
		}
	}

	// Positive S2 assertion: strict boot must have RECORDED the two lifecycle
	// logic constructs that fail to parse -- named, with their file + error --
	// rather than dropping them silently. This is the silent-drop the epic
	// targets, now visible in the structured report.
	if eng.loadReport == nil {
		t.Fatal("engine.loadReport should be set after Init")
	}
	for _, name := range []string{"driveDeploymentInProgress", "recordReconciledState"} {
		found := false
		for _, s := range eng.loadReport.Skipped {
			if s.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected the load report to name the skipped logic %q; report skips: %+v", name, eng.loadReport.Skipped)
		}
	}
}
