package memql

import (
	"log/slog"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestForgeProjectScopingAndPartitionIsolation is the #1792 conformance guard
// for the forge multi-project scoping and partition isolation model.
//
// It verifies three properties without requiring a running database:
//
//  1. queryProjectBySlug exists in the function registry and is bound to the
//     right concept (v1:forge:project) -- the slug->id resolver Claude uses
//     to go from a human project name to a projectId.
//
//  2. forgeResolveProject is present in the tool registry, is @mcp-tagged,
//     and its handler references queryProjectBySlug (wired correctly).
//
//  3. queryProjectRequests is bound to v1:forge:request -- confirming the
//     per-project filter chain (request.projectId -> project scoping) is
//     intact. This, combined with the engine's automatic partition WHERE
//     clause, implements the two-tier isolation model:
//
//       partition (engine-enforced, automatic)
//         └── project (payload.projectId filter in queryProjectRequests)
//               └── request rows
//
// The partition dimension is NOT a DSL filter -- the engine stamps
// WHERE partition = $envelope unconditionally. This test therefore does NOT
// assert a partition filter on the query; it asserts the concept binding so
// that we know the engine will route the query to the right partition table.
// A full DB-backed isolation test (two projects in the same partition, two
// partitions with the same slug) is tracked for #1793.
func TestForgeProjectScopingAndPartitionIsolation(t *testing.T) {
	// Load concepts first (required for concept-bound function resolution).
	if _, err := LoadUnifiedConcepts(slog.Default()); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := memoryNodes.DefaultRegistry()

	// Load functions.
	fnReg := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(slog.Default(), fnReg, conceptRegistry); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	// Load tools.
	toolReg := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), toolReg); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	}

	// --- 1. queryProjectBySlug -----------------------------------------------
	//
	// Must exist and be bound to the forge project concept. The slug field is
	// the required arg that uniquely identifies a project within a partition.
	qBySlug, err := fnReg.Get("queryProjectBySlug")
	if err != nil || qBySlug == nil {
		t.Fatalf("queryProjectBySlug missing from function registry (err=%v) -- the slug->id resolver is required for #1792", err)
	}
	if !strings.Contains(qBySlug.BoundConcept, "project") {
		t.Errorf("queryProjectBySlug.BoundConcept = %q, expected to reference the forge project concept", qBySlug.BoundConcept)
	}
	// Confirm the slug arg is declared (it is @required).
	if qBySlug.ArgsSchema == nil {
		t.Fatal("queryProjectBySlug.ArgsSchema is nil -- no args block parsed")
	}
	slugFound := false
	for _, f := range qBySlug.ArgsSchema.Fields {
		if f.Name == "slug" {
			slugFound = true
			break
		}
	}
	if !slugFound {
		t.Error("queryProjectBySlug.ArgsSchema missing 'slug' field -- the resolver arg is absent")
	}

	// --- 2. queryProjectRequests --------------------------------------------
	//
	// Must exist and be bound to the forge request concept. This is the
	// application-layer project scoping query (filter on payload.projectId).
	qProjReqs, err := fnReg.Get("queryProjectRequests")
	if err != nil || qProjReqs == nil {
		t.Fatalf("queryProjectRequests missing from function registry (err=%v)", err)
	}
	if !strings.Contains(qProjReqs.BoundConcept, "request") {
		t.Errorf("queryProjectRequests.BoundConcept = %q, expected to reference the forge request concept", qProjReqs.BoundConcept)
	}
	// Confirm projectId arg is declared.
	if qProjReqs.ArgsSchema == nil {
		t.Fatal("queryProjectRequests.ArgsSchema is nil")
	}
	pidFound := false
	for _, f := range qProjReqs.ArgsSchema.Fields {
		if f.Name == "projectId" {
			pidFound = true
			break
		}
	}
	if !pidFound {
		t.Error("queryProjectRequests.ArgsSchema missing 'projectId' field -- project scoping arg is absent")
	}

	// --- 3. forgeResolveProject tool ----------------------------------------
	//
	// Must be present, @mcp-tagged, and wired to queryProjectBySlug.
	resolverTool, err := toolReg.Get("forgeResolveProject")
	if err != nil || resolverTool == nil {
		t.Fatalf("forgeResolveProject missing from tool registry (err=%v) -- the MCP slug resolver is required for #1792", err)
	}
	if !resolverTool.MCPExposed {
		t.Error("forgeResolveProject is not @mcp-tagged -- it would be invisible to Claude via the MCP connector")
	}
	if resolverTool.Handler == nil {
		t.Fatal("forgeResolveProject.Handler is nil -- no handler wired")
	}
	if !strings.Contains(resolverTool.Handler.Query, "queryProjectBySlug") {
		t.Errorf("forgeResolveProject handler query = %q, expected to contain 'queryProjectBySlug' (the backing query)", resolverTool.Handler.Query)
	}
	// slug must appear in the JSON input schema.
	if !strings.Contains(string(resolverTool.InputSchema), "slug") {
		t.Error("forgeResolveProject InputSchema does not contain 'slug' -- the resolver arg is absent from the MCP tool schema")
	}
}
