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
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

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
