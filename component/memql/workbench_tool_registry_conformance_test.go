package memql

import (
	"io/fs"
	"log/slog"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestWorkbenchHostRegistersFromCarrierTree is the memql#1133 FIX-1 conformance
// guard.
//
// ROOT CAUSE (confirmed): `workbenchHost` -- the tool produceArtifact REQUIRES
// to write its deliverable to the per-Plan workbench -- is NOT defined in this
// engine repo's embedded DSL tree. It is a CARRIER tool, living in
// the carrier repo's pack tools file, and is mounted on the
// agent node at boot via `dsl.RegisterTree("<pack-domain>", carrierFS)` (the
// agent node is carrier-built per CLAUDE.md's "carrier-built vs engine-built"
// rule).
// So `workbenchHost` only lands in the resolved registry on a carrier build;
// the runaway's "toolsForToolCallingFiltered: tool not in registry" was the
// agent node failing to resolve it.
//
// This test reproduces the carrier mount path EXACTLY -- RegisterTree a
// pack-style overlay carrying a workbenchHost-shaped tool, then run the same
// LoadUnifiedTools the agent node runs -- and asserts workbenchHost resolves in
// the registry. It guards against any loader-skip / parse-drift regression that
// would silently drop the deliverable surface again (the loader logs+skips
// unparseable tool slices, so a future syntax change to the carrier tool could
// regress without this guard).
func TestWorkbenchHostRegistersFromCarrierTree(t *testing.T) {
	// A workbenchHost tool slice shaped like the carrier's pack tools
	// file definition (discriminated by `action`).
	const workbenchHostTool = `@enabled
@handler(type="builtin", builtin="workbenchDispatchHost")
@description("Run headless work in the per-Plan workbench sandbox: exec, fs_read, fs_write, fs_list, fs_stat, http_fetch.")
tool workbenchHost {
  action   string  @required @enum("exec", "fs_read", "fs_write", "fs_list", "fs_stat", "http_fetch") @description("The workbench action to perform")
  args     object  @description("Action-specific arguments")
  planId   string  @description("Auto-injected per-Plan workspace id")
  agentId  string  @description("Auto-injected owning agent id")
}
`
	overlay := fstest.MapFS{
		"tools.memql": {Data: []byte(workbenchHostTool)},
	}
	// Use a unique domain so the global plugin registry can't be contaminated
	// by (or contaminate) a parallel test. The real boot path uses the pack's
	// domain name; the loader walks every mounted domain identically, so
	// domain name does not affect tool resolution.
	const domain = "carrierconformance1133"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() {
		// Remove the throwaway overlay so its tools.memql does not leak into
		// later tests' dsl.Tree() walk. UnregisterTree is the clean teardown:
		// RegisterTree now fails loud on a duplicate domain (issue 2.4), so the
		// old "re-register the same domain to replace it" trick would panic.
		memqldsl.UnregisterTree(domain)
	})

	// Sanity: the overlay is reachable through the layered Tree() exactly the
	// way the carrier's pack domain is.
	if _, err := fs.ReadFile(memqldsl.Tree(), domain+"/tools.memql"); err != nil {
		t.Fatalf("overlay tools.memql not reachable via dsl.Tree(): %v", err)
	}

	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools failed: %v", err)
	}

	if !registry.Has("workbenchHost") {
		t.Fatalf("workbenchHost MUST be in the resolved tool registry once the "+
			"carrier tree is mounted -- it is the produceArtifact deliverable "+
			"surface (memql#1133). Registered tools: %v", registry.Names())
	}

	tool, err := registry.Get("workbenchHost")
	if err != nil {
		t.Fatalf("registry.Get(workbenchHost): %v", err)
	}
	if tool.Name != "workbenchHost" {
		t.Fatalf("resolved tool name = %q, want workbenchHost", tool.Name)
	}
	if len(tool.InputSchema) == 0 {
		t.Fatalf("workbenchHost resolved with an empty InputSchema -- the tool " +
			"loop's toolsForToolCallingFiltered would emit a useless definition")
	}
}
