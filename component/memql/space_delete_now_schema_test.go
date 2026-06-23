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

// deleteSpaceNowMutationSource mirrors the production
// `mutationDeleteSpaceNow` body in dsl/cognition/mutations.memql. Kept
// inline (rather than slicing the multi-construct file) so the exact
// insert shape under test is legible at a glance. The companion guard
// below is the contract: whatever the CoPresent SPA composes for the
// "Delete now" override MUST validate against the v1:cognition:space
// concept schema.
//
// memql#1059 regressed precisely because the SPA's delete-now payload
// carries `deleted: true` (the hard-delete tombstone the mutation's
// own doc-comment + the purgeExpiredArchivedSpaces cron + the
// traitIsNotDeleted filter all assume), yet the space concept --
// additionalProperties:false -- did not define the field, so the
// mutation failed:
//
//	additionalProperties 'deleted' not allowed
const deleteSpaceNowMutationSource = `use cognition.concepts.{ space }

mutation space mutationDeleteSpaceNow {
  args {
    partitionId  string  @required
    payload  object  @required
  }
  insert {
    id: args.partitionId
    ownerUserId: actor.userId
    args.payload
  }
}`

// compileSpaceSchema loads the REAL v1:cognition:space concept from the
// embedded DSL tree and returns its compiled definition schema, exactly
// the schema the production Concept.Create path validates against.
func compileSpaceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	concept, err := memoryNodes.DefaultRegistry().Get("v1:cognition:space")
	require.NoError(t, err, "v1:cognition:space concept must be loadable from the embedded DSL")

	raw, err := concept.DefinitionSchema()
	require.NoError(t, err)
	require.NotEmpty(t, raw, "space definition schema must be present")

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	require.NoError(t, compiler.AddResource("space.json", bytes.NewReader(raw)))
	schema, err := compiler.Compile("space.json")
	require.NoError(t, err)
	return schema
}

// renderDeleteSpaceNowPayload renders the delete-now mutation with the
// exact payload the CoPresent SPA composes (status=archived,
// active=false, deleted=true) into the concrete insert payload -- the
// same JSON the engine hands to Concept.Create.
func renderDeleteSpaceNowPayload(t *testing.T) map[string]any {
	t.Helper()

	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
	fn, err := tryParseNewFunctionSyntax(
		"mutationDeleteSpaceNow", "mutation",
		deleteSpaceNowMutationSource, "dsl/cognition/mutations.memql", registry,
	)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:cognition:space", fn.MutationTemplate.Concept)

	ctx := context.Background()
	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"partitionId": "v1:cognition:space:quarterly-review",
		"payload": map[string]any{
			"ownerUserId": "user-archivist",
			"name":        "Quarterly Review",
			"status":      "archived",
			"active":      false,
			"deleted":     true,
		},
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	return payload
}

// TestDeleteSpaceNowPayloadValidates pins the fix for memql#1059: the
// SPA's "Delete now" payload -- which sets deleted=true -- must validate
// against the real v1:cognition:space concept schema. Before the fix the
// concept had no `deleted` field and (additionalProperties:false)
// rejected it.
func TestDeleteSpaceNowPayloadValidates(t *testing.T) {
	schema := compileSpaceSchema(t)
	payload := renderDeleteSpaceNowPayload(t)

	require.NoError(t, schema.Validate(payload),
		"delete-now space payload (deleted=true) must validate against v1:cognition:space (memql#1059)")

	require.Equal(t, true, payload["deleted"])
	require.Equal(t, false, payload["active"])
	require.Equal(t, "archived", payload["status"])
}

// TestSpaceSchemaAllowsDeletedField proves the `deleted` field is a
// genuine, schema-recognised space field -- i.e. the fix added it to the
// concept rather than the payload merely slipping past validation. This
// is the exact failure mode reported in memql#1059:
//
//	additionalProperties 'deleted' not allowed
//
// A space concept that drops the `deleted` field again will fail here,
// guarding the SPA <-> concept contract from re-drifting.
func TestSpaceSchemaAllowsDeletedField(t *testing.T) {
	schema := compileSpaceSchema(t)

	// A minimal valid space carrying only the tombstone marker beyond
	// the required fields must validate.
	base := map[string]any{
		"ownerUserId": "user-archivist",
		"name":        "Quarterly Review",
		"deleted":     true,
	}
	require.NoError(t, schema.Validate(base),
		"v1:cognition:space must accept the `deleted` tombstone field (memql#1059)")

	// And a genuinely-unknown field is still rejected -- proving the
	// schema is the gate and `deleted` is allow-listed, not a side effect
	// of additionalProperties being relaxed.
	base["totallyNotAField"] = true
	err := schema.Validate(base)
	require.Error(t, err, "space schema must still reject stray fields")
	require.Contains(t, err.Error(), "totallyNotAField",
		"rejection must name the offending additionalProperty")
}
