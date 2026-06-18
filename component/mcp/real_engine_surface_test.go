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
