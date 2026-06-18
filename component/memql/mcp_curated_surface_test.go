package memql

import (
	"log/slog"
	"sort"
	"testing"
)

// TestMCPCuratedToolSurface is the #1596/#1597 conformance guard: it pins
// exactly which engine `tool {}` constructs are exposed to the MCP connector
// via the @mcp allowlist. The MCP node reflects only @mcp-tagged tools into
// tools/list once any tool carries the flag (component/mcp/tool_surface.go);
// this test loads the real engine DSL and asserts the curated set is tagged
// (MCPExposed) and that infra/uncurated tools are NOT -- so an accidental
// retag (either direction) fails CI instead of silently widening or emptying
// the connector surface.
func TestMCPCuratedToolSurface(t *testing.T) {
	// The intentional curated `tool {}` surface (#1596). Queries promoted
	// with @mcp (queryCurrentUser, query*ById, ...) ride the separate
	// MCPPromotedFunctionTools path and are not in this registry, so they
	// are out of scope here.
	curated := []string{
		"notesCreate", "notesList", "notesSearch", "notesUpdate",
		"todosCreate", "todosList", "todosUpdate", "todosComplete",
		"calendarCreate", "calendarList", "calendarFind", "calendarUpdate", "calendarDelete",
		"searchUsers",
	}

	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools failed: %v", err)
	}

	// Every curated tool must be present AND @mcp-tagged.
	for _, name := range curated {
		tool, err := registry.Get(name)
		if err != nil {
			t.Fatalf("curated tool %q missing from the engine registry: %v", name, err)
		}
		if !tool.MCPExposed {
			t.Errorf("curated tool %q is not @mcp-tagged (MCPExposed=false) -- it would be dropped from the connector surface", name)
		}
	}

	// describeFunction is an introspection meta-tool, intentionally EXCLUDED
	// from the connector (#1597) -- it must stay untagged.
	if tool, err := registry.Get("describeFunction"); err == nil && tool.MCPExposed {
		t.Errorf("describeFunction is @mcp-tagged but should be excluded from the curated connector surface (#1597)")
	}

	// The set of @mcp-tagged engine tools must be EXACTLY the curated set --
	// no more (a stray tag widens the surface), no fewer (a dropped tag
	// shrinks it). Catches accidental over/under-tagging.
	var exposed []string
	for _, tool := range registry.List() {
		if tool != nil && tool.MCPExposed {
			exposed = append(exposed, tool.Name)
		}
	}
	sort.Strings(exposed)
	want := append([]string(nil), curated...)
	sort.Strings(want)
	if len(exposed) != len(want) {
		t.Fatalf("@mcp-tagged engine tools = %v (n=%d), want exactly the curated set %v (n=%d)", exposed, len(exposed), want, len(want))
	}
	for i := range want {
		if exposed[i] != want[i] {
			t.Fatalf("@mcp-tagged engine tools = %v, want exactly %v", exposed, want)
		}
	}
}
