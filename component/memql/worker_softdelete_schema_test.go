package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// softDeleteWorkerInvocationMutationSource mirrors the production
// `mutationSoftDeleteWorkerInvocation` body in dsl/worker/mutations.memql.
// Kept inline (rather than slicing the multi-construct file) so the exact
// update shape under test is legible at a glance. The companion guard below
// is the contract: the retention sweep stamps `deleted: true`, and that
// write MUST validate against the v1:worker:invocation concept schema.
//
// memql#1644 regressed precisely because this update carries `deleted: true`
// (the soft-delete flag the mutation's own doc-comment + the
// workerInvocationRetentionSweep cron + the traitIsNotDeleted filter all
// assume), yet the invocation concept -- additionalProperties:false -- did
// not define the field, so the mutation failed:
//
//	additionalProperties 'deleted' not allowed
const softDeleteWorkerInvocationMutationSource = `use worker.concepts.{ invocation }

mutation invocation mutationSoftDeleteWorkerInvocation {
  args {
    invocationId  string  @required
  }
  update {
    id: args.invocationId
    deleted: true
  }
}`

// compileWorkerInvocationSchema loads the REAL v1:worker:invocation concept
// from the embedded DSL tree and returns its compiled definition schema --
// exactly the schema the production Concept.Create/Update path validates
// against.
func compileWorkerInvocationSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	concept, err := memoryNodes.DefaultRegistry().Get("v1:worker:invocation")
	require.NoError(t, err, "v1:worker:invocation concept must be loadable from the embedded DSL")

	raw, err := concept.DefinitionSchema()
	require.NoError(t, err)
	require.NotEmpty(t, raw, "invocation definition schema must be present")

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	require.NoError(t, compiler.AddResource("invocation.json", bytes.NewReader(raw)))
	schema, err := compiler.Compile("invocation.json")
	require.NoError(t, err)
	return schema
}

// renderSoftDeleteWorkerInvocationPayload renders the soft-delete mutation
// with the exact arg the retention sweep passes, producing the concrete
// update payload the engine merges into the stored row.
func renderSoftDeleteWorkerInvocationPayload(t *testing.T) map[string]any {
	t.Helper()

	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:worker:invocation": {Name: "v1:worker:invocation"},
	})
	fn, err := tryParseNewFunctionSyntax(
		"mutationSoftDeleteWorkerInvocation", "mutation",
		softDeleteWorkerInvocationMutationSource, "dsl/worker/mutations.memql", registry,
	)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:worker:invocation", fn.MutationTemplate.Concept)

	ctx := context.Background()
	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"invocationId": "v1:worker:invocation:abc123",
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	return payload
}

// TestSoftDeleteWorkerInvocationRendersDeletedField proves the soft-delete
// mutation body stamps `deleted: true` -- the write the retention sweep
// relies on.
func TestSoftDeleteWorkerInvocationRendersDeletedField(t *testing.T) {
	payload := renderSoftDeleteWorkerInvocationPayload(t)
	require.Equal(t, true, payload["deleted"],
		"mutationSoftDeleteWorkerInvocation must stamp deleted=true")
}

// TestWorkerInvocationSchemaAllowsDeletedField pins the fix for memql#1644:
// the `deleted` soft-delete flag must be a genuine, schema-recognised
// v1:worker:invocation field. Before the fix the concept had no `deleted`
// field and (additionalProperties:false) rejected the soft-delete write:
//
//	additionalProperties 'deleted' not allowed
//
// An invocation concept that drops the `deleted` field again will fail here,
// guarding the retention-sweep <-> concept contract from re-drifting.
func TestWorkerInvocationSchemaAllowsDeletedField(t *testing.T) {
	schema := compileWorkerInvocationSchema(t)

	// A minimal valid invocation carrying the soft-delete marker beyond the
	// required fields must validate.
	base := map[string]any{
		"ownerUserId": "user-owner",
		"workerId":    "v1:worker:registration:wk1",
		"agentId":     "v1:agents:agent:a1",
		"tool":        "workerHost",
		"action":      "exec",
		"startedAt":   "2026-06-17T12:00:00Z",
		"outcome":     "success",
		"deleted":     true,
	}
	require.NoError(t, schema.Validate(base),
		"v1:worker:invocation must accept the `deleted` soft-delete field (memql#1644)")

	// And a genuinely-unknown field is still rejected -- proving the schema
	// is the gate and `deleted` is allow-listed, not a side effect of
	// additionalProperties being relaxed.
	base["totallyNotAField"] = true
	err := schema.Validate(base)
	require.Error(t, err, "invocation schema must still reject stray fields")
	require.Contains(t, err.Error(), "totallyNotAField",
		"rejection must name the offending additionalProperty")
}
