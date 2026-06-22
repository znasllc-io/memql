package memql

// reference_pack_load_test.go is the in-package half of the issue 2.5 reference
// pack verification. It proves the pack's tool resolves into the engine's
// ToolRegistry via the same LoadUnifiedTools the agent node runs at boot --
// i.e. the pack EXTENDS the core tool surface.
//
// Why this lives here (package memql) and not in examples/referencepack:
// LoadUnifiedTools needs a *ToolRegistry, built by the unexported
// newToolRegistry constructor, which an external test package cannot call. We
// also cannot import the examples/referencepack Go package from a component/memql
// test without an import cycle (examples/referencepack imports component/memql).
// So this test mounts the pack's REAL tools.memql file (read from disk) as an
// overlay -- exercising the actual pack artifact through the loader without the
// Go-package dependency. The companion examples/referencepack/reference_pack_test.go
// proves the concept-load + provider-capability + contract halves via exported
// API. Together they prove load + extend under the default `go test ./...`.

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql/dslimports"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestReferencePackToolResolvesInRegistry mounts the reference pack's real
// tools.memql under a unique domain and asserts the unified tool loader resolves
// the pack's tool into the registry -- the pack extending the core tool surface.
func TestReferencePackToolResolvesInRegistry(t *testing.T) {
	// Read the pack's ACTUAL tool definition from disk so this test exercises
	// the real artifact (a parse/syntax regression in the pack's tools.memql
	// fails this test). The path is relative to this test file's package dir
	// (component/memql) up to the repo root, then into the pack.
	toolPath := filepath.Join("..", "..", "examples", "referencepack", "dsl", "tools.memql")
	toolSrc, err := os.ReadFile(toolPath)
	if err != nil {
		t.Fatalf("reading reference pack tools.memql at %s: %v", toolPath, err)
	}

	overlay := fstest.MapFS{
		"tools.memql": {Data: toolSrc},
	}
	// Unique throwaway domain: RegisterTree fails loud on a duplicate domain
	// (issue 2.4), so register under a unique name + UnregisterTree teardown.
	const domain = "referencepacktoolload"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools failed: %v", err)
	}

	const wantTool = "referencePackGreet"
	if !registry.Has(wantTool) {
		t.Fatalf("pack tool %q MUST resolve in the tool registry once the pack "+
			"tree is mounted -- the pack failed to extend the core tool surface. "+
			"Registered tools: %v", wantTool, registry.Names())
	}

	tool, err := registry.Get(wantTool)
	if err != nil {
		t.Fatalf("registry.Get(%s): %v", wantTool, err)
	}
	if tool.Name != wantTool {
		t.Fatalf("resolved tool name = %q, want %s", tool.Name, wantTool)
	}
	if len(tool.InputSchema) == 0 {
		t.Fatalf("pack tool %q resolved with an empty InputSchema", wantTool)
	}
}

// TestReferencePackBuiltinLoads loads the pack's real builtins.memql through the
// unified builtin loader and asserts the pack's builtin registers -- proving the
// @executor("integration.referencepack.composeGreeting") wiring parses.
func TestReferencePackBuiltinLoads(t *testing.T) {
	builtinPath := filepath.Join("..", "..", "examples", "referencepack", "dsl", "builtins.memql")
	builtinSrc, err := os.ReadFile(builtinPath)
	if err != nil {
		t.Fatalf("reading reference pack builtins.memql: %v", err)
	}
	overlay := fstest.MapFS{"builtins.memql": {Data: builtinSrc}}
	const domain = "referencepackbuiltinload"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins failed: %v", err)
	}
	if _, err := registry.Get("referencePackComposeGreeting"); err != nil {
		t.Fatalf("pack builtin referencePackComposeGreeting MUST register: %v", err)
	}
}

// TestReferencePackDSLTreeParses runs the full import-resolving DSL loader over
// the pack's ENTIRE on-disk dsl/ tree (concept + builtin + tool + automation,
// including the automation's `use referencepack.builtins.{...}` import). This is
// the strongest single proof that every pack DSL artifact parses and its
// cross-file imports resolve coherently -- in particular the automation that
// HOOKS the core v1:cognition:space service event.
func TestReferencePackDSLTreeParses(t *testing.T) {
	// dslimports.Load resolves `use` imports against the tree's domain layout:
	// `use referencepack.builtins.{...}` maps to referencepack/builtins.memql.
	// The pack's files live in examples/referencepack/dsl/, but at runtime they
	// mount under referencepack/ (dsl.RegisterTree("referencepack", Tree())). So
	// build a root that mirrors that mount: every dsl/<f>.memql -> referencepack/<f>.memql.
	packDSL := filepath.Join("..", "..", "examples", "referencepack", "dsl")
	entries, err := os.ReadDir(packDSL)
	if err != nil {
		t.Fatalf("reading pack dsl dir: %v", err)
	}
	root := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(packDSL, e.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", e.Name(), readErr)
		}
		root["referencepack/"+e.Name()] = &fstest.MapFile{Data: data}
	}

	if _, err := dslimports.Load(fs.FS(root)); err != nil {
		t.Fatalf("dslimports.Load over the pack tree MUST succeed -- a parse or "+
			"import-resolution failure here means a pack DSL artifact (concept, "+
			"builtin, tool, or the core-service-hook automation) is broken: %v", err)
	}
}
