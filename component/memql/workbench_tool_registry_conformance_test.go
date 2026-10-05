package memql

import (
	"io/fs"
	"log/slog"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// The engine ships its workbench tool. A mounted bundle may declare another
// tool with the same leaf name; both must remain reachable by namespace.
func TestWorkbenchHostRegistersFromCarrierTree(t *testing.T) {
	// A workbenchHost tool slice shaped like the carrier's pack tools
	// file definition (discriminated by `action`).
	// The handler reads `@handler(type="function", name="workbenchDispatchHost")`
	// because that is the spelling that WORKS. This fixture carried
	// `@handler(type="builtin", builtin="workbenchDispatchHost")` until
	// memql#3625 made a tool declaration checkable, and neither half of that
	// existed: `builtin` is not a @handler argument (so the value was dropped,
	// leaving no target), and "builtin" is not a handler TYPE (executeToolHandler
	// dispatches query / function / webhook and errors on anything else). The
	// tool registered, resolved by name -- which is all this test asserted --
	// and would have failed the moment an agent actually called it.
	//
	// A builtin IS reached through the function handler: builtins are Functions
	// internally and share the FunctionRegistry, which is what makes
	// `workbenchDispatchHost` (dsl/workbench/builtins.memql) resolvable here.
	const workbenchHostTool = `
@handler(type="function", name="workbenchDispatchHost")
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
	memqldsl.RegisterTree(domain, withLanguageLine(overlay))
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

	if !registry.Has(domain + ".workbenchHost") {
		t.Fatalf("workbenchHost MUST be in the resolved tool registry once the "+
			"carrier tree is mounted -- it is the produceArtifact deliverable "+
			"surface (memql#1133). Registered tools: %v", registry.Names())
	}

	if !registry.Has("workbench.workbenchHost") {
		t.Fatal("engine workbench tool is missing")
	}
	tool, err := registry.Get(domain + ".workbenchHost")
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
