package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Regression coverage for memql#1633: several mutations silently dropped
// caller-supplied args that weren't declared in their args { ... } block (or
// weren't forwarded into the insert/update body), losing valid data with no
// error -- and in createTask's case leaving a @required concept field
// unsatisfied. The fix is two-pronged:
//
//  1. Wire the missing args through (args block + insert/update body) so the
//     caller value reaches the concept payload.
//  2. Reject unknown args at the MCP boundary instead of silently dropping
//     them (rejectUnknownArgs, gated by WithStrictUnknownArgs).
//
// These tests load the REAL DSL tree so they fail against the pre-fix
// mutations and pass with the wire-through + strict-reject in place.

// load1633Functions loads concepts + functions from the embedded DSL tree.
func load1633Functions(t *testing.T) *FunctionRegistry {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)
	return fnRegistry
}

// renderPayload renders a loaded mutation's template with args and returns the
// decoded insert/update payload. A concepts-less engine makes
// canonicalizeRelationshipFields a no-op, so no DB is needed.
func renderPayload(t *testing.T, fnRegistry *FunctionRegistry, name string, args map[string]any) map[string]any {
	t.Helper()
	fn, err := fnRegistry.Get(name)
	require.NoError(t, err, "%s must load from the DSL tree", name)
	require.NotNil(t, fn.MutationTemplate, "%s must have a mutation template", name)

	e := &MemQLEngine{} // concepts nil -> canonicalize is a no-op
	mutation, err := e.renderMutationTemplate(context.Background(), fn.MutationTemplate, args)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	return payload
}

func TestDroppedArgs1633_WireThrough(t *testing.T) {
	reg := load1633Functions(t)

	t.Run("createTask carries category", func(t *testing.T) {
		payload := renderPayload(t, reg, "createTask", map[string]any{
			"taskId": "t1", "planId": "p1", "category": "toolInvocation",
			"kind": "callTool", "seq": int64(0), "input": map[string]any{},
		})
		require.Equal(t, "toolInvocation", payload["category"],
			"caller-supplied category must reach the v1:planner:task payload (#1633)")
	})

	t.Run("createTask defaults category to semantic", func(t *testing.T) {
		payload := renderPayload(t, reg, "createTask", map[string]any{
			"taskId": "t2", "planId": "p1", "kind": "llmAnalyze",
			"seq": int64(1), "input": map[string]any{},
		})
		require.Equal(t, "semantic", payload["category"],
			"category is @required on the concept; absent arg must default to semantic")
	})

	t.Run("createNode carries capabilities", func(t *testing.T) {
		payload := renderPayload(t, reg, "createNode", map[string]any{
			"id": "n1", "nodeType": "bff", "capabilities": []any{"http"},
		})
		require.Equal(t, []any{"http"}, payload["capabilities"],
			"caller-supplied capabilities must reach the v1:cluster:node payload (#1633)")
	})

	t.Run("createSpawnEvent carries initiatorId and metadata", func(t *testing.T) {
		payload := renderPayload(t, reg, "createSpawnEvent", map[string]any{
			"nodeId": "n1", "nodeType": "bff", "action": "spawned",
			"initiatorId": "n0", "metadata": map[string]any{"k": "v"},
		})
		require.Equal(t, "n0", payload["initiatorId"], "initiatorId must persist (#1633)")
		require.Equal(t, map[string]any{"k": "v"}, payload["metadata"], "metadata must persist (#1633)")
	})

	t.Run("createCluster honors supplied status", func(t *testing.T) {
		payload := renderPayload(t, reg, "createCluster", map[string]any{
			"name": "staging", "environment": "staging", "status": "degraded",
		})
		require.Equal(t, "degraded", payload["status"],
			"caller-supplied status must override the healthy default (#1633)")
	})

	t.Run("createCluster defaults status to healthy", func(t *testing.T) {
		payload := renderPayload(t, reg, "createCluster", map[string]any{
			"name": "staging", "environment": "staging",
		})
		require.Equal(t, "healthy", payload["status"])
	})

	t.Run("markChunkSuperseded maps reason to supersededReason", func(t *testing.T) {
		payload := renderPayload(t, reg, "markChunkSuperseded", map[string]any{
			"chunkId": "c1", "supersededAt": "2026-01-01T00:00:00Z",
			"reason": "superseded by 2026 figures",
		})
		require.Equal(t, "superseded by 2026 figures", payload["supersededReason"],
			"`reason` must land on the concept's supersededReason field (#1633)")
	})

	t.Run("setUserActiveSpace persists partitionId", func(t *testing.T) {
		payload := renderPayload(t, reg, "setUserActiveSpace", map[string]any{
			"userId": "u1", "partitionId": "s1",
		})
		require.Equal(t, "s1", payload["activePartitionId"])
	})

	t.Run("setUserActiveSpace persists activePartitionId alias", func(t *testing.T) {
		payload := renderPayload(t, reg, "setUserActiveSpace", map[string]any{
			"userId": "u1", "activePartitionId": "s2",
		})
		require.Equal(t, "s2", payload["activePartitionId"],
			"the activePartitionId arg name (1-Identity QA) must also land (#1633)")
	})
}

func TestDroppedArgs1633_RejectUnknownArgs(t *testing.T) {
	reg := load1633Functions(t)

	fn, err := reg.Get("createTask")
	require.NoError(t, err)

	// A now-declared arg is accepted.
	require.NoError(t, rejectUnknownArgs(fn, map[string]any{
		"planId": "p1", "category": "semantic", "kind": "k", "seq": int64(0), "input": map[string]any{},
	}))

	// An undeclared arg is rejected, naming the offending arg.
	err = rejectUnknownArgs(fn, map[string]any{
		"planId": "p1", "kind": "k", "seq": int64(0), "input": map[string]any{}, "bogusArg": "x",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogusArg")
}

// TestDroppedArgs1633_StrictGateOnlyAtMCP confirms the unknown-arg rejection
// fires only when WithStrictUnknownArgs is set (the MCP boundary), so internal
// engine callers stay lenient. With strict on, an undeclared arg errors before
// any DB work; with strict off, it falls through to the (DB-needing) render and
// does NOT error on the unknown arg.
func TestDroppedArgs1633_StrictGateOnlyAtMCP(t *testing.T) {
	reg := load1633Functions(t)
	e := &MemQLEngine{} // concepts/db nil

	call := &FunctionCallExpression{
		Name: "createTask",
		Args: map[string]any{"bogusArg": "x"},
	}

	_, err := e.executeMutationFunctionCall(WithStrictUnknownArgs(context.Background()), call, reg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogusArg",
		"strict MCP path must reject the unknown arg before touching the DB")

	// Without the strict flag the unknown arg is not the failure cause: the
	// lenient path proceeds past arg validation (and fails later for an
	// unrelated reason -- missing required fields / no DB -- never mentioning
	// the unknown arg).
	_, lenientErr := e.executeMutationFunctionCall(context.Background(), call, reg)
	if lenientErr != nil {
		require.NotContains(t, lenientErr.Error(), "bogusArg",
			"internal (non-MCP) callers must stay lenient about unknown args")
	}
}
