//go:build mcp

package mcp

// real_engine_surface_test.go -- the deployed-style smoke assertion for the
// resources + prompts surface (#1598). The existing resources_test.go drives a
// hand-built fakeResourceEngine, so it would stay green even if the REAL engine
// path were broken: the deployed mcp node passes a concrete *memql.MemQLEngine
// to NewServer, and the server reaches it via asResourceEngine(s.engine), which
// is a plain interface type-assertion. If that bridge ever stopped being
// satisfied (or the engine loaded zero concepts/prompts under the mcp build),
// resources/list + prompts/list would silently come back empty -- exactly the
// 0-resources/0-prompts symptom #1598 chased on staging.
//
// This test wires a FULLY-LOADED real engine (the same New + LoadUnifiedConcepts
// + Init bootstrap the cluster runs, DB-free) into a real Server and asserts the
// JSON-RPC resources/list + prompts/list handlers return non-empty surfaces -- so
// it FAILS if the ResourceEngine bridge is nil/empty and PASSES when wired.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// loadedEngine builds a real *memql.MemQLEngine with the full embedded DSL tree
// loaded (concepts + specs/shapes/functions/builtins/tools/prompts/providers),
// mirroring the engine bootstrap in component/memql/engine_load_smoke_test.go.
// No Postgres is required: the DSL load + validation is DB-free.
func loadedEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts; DSL tree did not load")
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	// Quiet the provider loader (one WARN per provider when no secrets are seeded;
	// expected here, not what this test checks).
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree failed: %v", err)
	}
	return eng
}

// rpcList drives one JSON-RPC list request through the real server and returns
// the named result slice.
func rpcList(t *testing.T, s *Server, method, key string) []any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	raw, _ := json.Marshal(req)
	resp := s.handleMessage(context.Background(), raw)
	if resp == nil {
		t.Fatalf("%s returned no response", method)
	}
	if resp.Error != nil {
		t.Fatalf("%s returned a protocol error: %+v", method, resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("%s result is not an object: %T", method, resp.Result)
	}
	list, _ := result[key].([]any)
	if list == nil {
		// conceptResources/promptDefinitions return []map[string]any; the route
		// nests them under the key without re-typing, so handle both shapes.
		if typed, ok := result[key].([]map[string]any); ok {
			out := make([]any, len(typed))
			for i := range typed {
				out[i] = typed[i]
			}
			return out
		}
	}
	return list
}

// TestRealEngineExposesResourcesAndPrompts is the #1598 smoke assertion: over a
// real, fully-loaded engine the deployed-style surface must expose >0 resources
// AND >0 prompts. This is what the connector hits as resources/list +
// prompts/list. It fails if asResourceEngine(s.engine) is nil (bridge not wired)
// or if the engine carries no concepts/prompts under the mcp build.
func TestRealEngineExposesResourcesAndPrompts(t *testing.T) {
	eng := loadedEngine(t)

	// Guard the seam directly first: the real engine MUST satisfy the
	// ResourceEngine bridge the server adapts s.engine through. A nil here is the
	// exact failure mode #1598 suspected on the deployed node.
	if asResourceEngine(eng) == nil {
		t.Fatal("asResourceEngine(*memql.MemQLEngine) returned nil: the ResourceEngine bridge is not satisfied -- resources/list + prompts/list would be empty on the deployed mcp node (#1598)")
	}

	s := NewServer(slog.Default(), "memql-mcp", "test", eng, Config{})

	resources := rpcList(t, s, "resources/list", "resources")
	if len(resources) == 0 {
		t.Fatal("resources/list returned 0 resources over a fully-loaded engine: the concept-resource surface is empty (#1598)")
	}

	prompts := rpcList(t, s, "prompts/list", "prompts")
	if len(prompts) == 0 {
		t.Fatal("prompts/list returned 0 prompts over a fully-loaded engine: the DSL-prompt surface is empty (#1598)")
	}

	t.Logf("real-engine MCP surface: %d resources, %d prompts", len(resources), len(prompts))
}

// TestRealEngineCuratedToolSurface pins the curated tool-surface decision
// over a real, fully-loaded engine: describeFunction (function/arg
// introspection) is in the curated @mcp allowlist because it materially
// improves MCP-client usability; searchUsers (already @mcp) stays in. This
// is the #1596/#1646 gate.
//
// recentChat (the space-chat lens, #1684) was in this surface until #1976
// moved it to the product pack (the pack's tools file) -- it binds the
// chat integration's recentChat builtin, a product capability, so
// it is no longer in the engine-only core surface loadedEngine builds. The
// carrier build (pack registered) re-adds it; the pack covers its @mcp tag.
func TestRealEngineCuratedToolSurface(t *testing.T) {
	eng := loadedEngine(t)

	names := map[string]bool{}
	for _, m := range listMCPTools(asEngine(eng), "assistant", TierSealed) {
		if n, _ := m["name"].(string); n != "" {
			names[n] = true
		}
	}

	if !names["describeFunction"] {
		t.Errorf("describeFunction must be in the curated MCP surface (#1646); got tools %v", names)
	}
	if !names["searchUsers"] {
		t.Errorf("searchUsers (already @mcp) must remain in the curated MCP surface; got tools %v", names)
	}
}

// TestWorkJournalIsNotReachableOverMCP is what became of
// TestHarnessStepChainReachableOverMCP when the work spine retired the harness
// (epic memql#4962).
//
// THE ORIGINAL PROPERTY (memql#1679) WAS: a mutation chain an agent must call
// IN SEQUENCE has to be promoted WHOLE, because a half-promoted chain is worse
// than none -- the agent gets to `pending` and stops. Its subject was the seven
// harness step-chain mutations, and all seven are deleted. There is no promoted
// mutation chain left in the tree to re-point it at: every @mcp-promoted
// function today is a READ.
//
// SO THE TEST IS INVERTED RATHER THAN DELETED, because the work spine gives the
// same question a new and sharper answer. The journal's writes are the engine's
// testimony about its own execution: a caller who can insert a run or a step row
// can claim an execution that never happened, and resume, replay and recall will
// serve that forgery back as fact. Every one of them is @serverOnly for exactly
// that reason -- so the property worth guarding is that NONE of them is
// reachable over the connector, which is the opposite of what #1679 asked and
// the same kind of claim.
//
// The promoted set is asserted NON-EMPTY first. Without that, a build in which
// promotion is broken outright would satisfy every absence check below while
// measuring nothing.
func TestWorkJournalIsNotReachableOverMCP(t *testing.T) {
	eng := loadedEngine(t)

	promoted := map[string]bool{}
	for _, m := range eng.MCPPromotedFunctionTools() {
		if n, _ := m["name"].(string); n != "" {
			promoted[n] = true
		}
	}
	if len(promoted) == 0 {
		t.Fatal("no functions are @mcp-promoted at all, so the absence checks below prove nothing " +
			"-- promotion itself is broken, or loadedEngine did not load the tree")
	}

	s := NewServer(slog.Default(), "memql-mcp", "test", eng, Config{})
	toolNames := map[string]bool{}
	for _, raw := range rpcList(t, s, "tools/list", "tools") {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if n, _ := m["name"].(string); n != "" {
			toolNames[n] = true
		}
	}
	if len(toolNames) == 0 {
		t.Fatal("tools/list came back empty, so the absence checks below prove nothing")
	}

	// The journal's writers, plus the two reads resume makes under the
	// synthetic cluster actor. A caller reaching any of these is either
	// forging an execution record or reading another tenant's run.
	sealed := []string{
		"createWorkRun",
		"updateWorkRun",
		"createWorkStep",
		"updateWorkStep",
		"createWorkGoal",
		"createWorkModelCall",
		"createWorkObservation",
		"createWorkApproval",
		"decideWorkApproval",
		"workRunById",
		"workStepsForRun",
	}
	for _, name := range sealed {
		if promoted[name] {
			t.Errorf("%q is @mcp-promoted. It is @serverOnly: a client-reachable write here forges an "+
				"execution record that resume and replay then serve back as fact (work spine, epic memql#4962)", name)
		}
		if toolNames[name] {
			t.Errorf("%q appears in tools/list over the MCP server, so it is callable by a connected agent", name)
		}
	}

	// The owner-facing reads are @actor and caller-scoped rather than
	// @serverOnly, so they are ALLOWED to be promoted -- this test must not
	// grow into "nothing in the work namespace may ever be reachable".
	t.Logf("work journal MCP surface: %d sealed constructs absent from a %d-tool surface",
		len(sealed), len(toolNames))
}
