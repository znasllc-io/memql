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
	} {
		if _, err := registry.Get(name); err != nil {
			t.Errorf("deploy pack builtin %q MUST register (its @executor wiring must parse): %v", name, err)
		}
	}
}

// TestDeployPackLifecycleAutomationLoads is the E2.3 (#2096) load proof: mount
// the deploy pack's ENTIRE dsl/ tree (builtins + the lifecycle automation +
// its logic) under the deploypack domain and run the SAME full engine Init the
// cluster runs at boot. This validates that:
//   - the driveDeploymentInProgress automation + logic parse,
//   - the logic's `use deploypack.builtins.{ deployRunPromote }` resolves to
//     the pack's own effect, AND
//   - the logic's `use cluster.mutations.{ updateDeploymentStatus }` resolves
//     to the CORE cluster mutation (the cross-namespace import the port relies
//     on to transition the deployment).
//
// A parse, import-resolution, or step-registry regression in the live-deploy
// orchestration port fails here instead of crashing the deploypack-tagged
// binary at boot. Mounted from disk (not via a Go import of examples/deploypack,
// which would be an import cycle) so it exercises the real pack artifacts.
func TestDeployPackLifecycleAutomationLoads(t *testing.T) {
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
		t.Fatalf("engine.Init over the core tree + mounted deploy pack failed -- "+
			"the deploy-lifecycle automation/logic or its imports do not resolve: %v", err)
	}

	// The pack's effect builtins the lifecycle logic fires must be registered
	// as functions after Init (a successful Init over the mounted tree already
	// proves the automation + logic parsed and their imports -- including the
	// core `cluster.mutations.{ updateDeploymentStatus }` cross-namespace
	// import -- resolved; logic bodies themselves live in the automation/step
	// registry, not the function registry, so we assert on the effect builtin
	// the logic depends on).
	fns := eng.Functions()
	for _, name := range []string{
		"deployRunPromote",       // the azure effect the lifecycle logic fires
		"updateDeploymentStatus", // the core mutation it transitions through
	} {
		if !fns.Has(name) {
			t.Errorf("expected %q to be registered after Init over the mounted deploy pack", name)
		}
	}
}
