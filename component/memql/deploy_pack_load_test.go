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
// STRICT-BOOT HISTORY (epic #2351 / S2 #2357, fixed by #2400): strict boot
// revealed that the pack's two lifecycle LOGIC constructs had NEVER parsed
// (bare-expression `:=` steps the logic-step grammar rejects) -- silently
// skipped for their entire life while this test stayed green by asserting
// only on builtins. #2400 rewrote both logics to the current grammar (call /
// if-cond-call steps; graph writes moved out to the automations' gated
// mutation steps per the behavioral-constructs ADR) and this test now runs
// STRICT: full Init with zero skips, and the logics themselves must register.
//
// Mounted from disk (not via a Go import of examples/deploypack, which would be
// an import cycle) so it exercises the real pack artifacts.
func TestDeployPackLifecycleAutomationLoads(t *testing.T) {
	// STRICT (no break-glass, #2400): the lifecycle logics were rewritten to
	// the current logic-step grammar (call / if-cond-call step forms; writes
	// moved out to the automations' gated mutation steps), so the pack must
	// now survive strict boot with ZERO skips.

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
			"under STRICT boot -- a pack construct stopped parsing: %v", err)
	}

	// The pack's effect builtins the lifecycle logic fires must be registered
	// as functions after Init, and so must the core cross-namespace mutation.
	fns := eng.Functions()
	for _, name := range []string{
		"deployRunPromote",          // the azure effect the lifecycle logic fires
		"updateDeploymentStatus",    // the core mutation the automations persist through
		"driveDeploymentInProgress", // the lifecycle logic itself (#2400: never parsed before)
		"recordReconciledState",     // the record-back logic (#2400: never parsed before)
	} {
		if !fns.Has(name) {
			t.Errorf("expected %q to be registered after Init over the mounted deploy pack", name)
		}
	}

	// Strict-boot invariant (#2400): the load report carries ZERO skips for
	// the mounted pack -- the pre-#2400 silent-drop class stays dead.
	if eng.loadReport == nil {
		t.Fatal("engine.loadReport should be set after Init")
	}
	if len(eng.loadReport.Skipped) != 0 {
		t.Errorf("expected zero skipped constructs under strict boot; report skips: %+v", eng.loadReport.Skipped)
	}
}
