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

// greetingMutationSource mirrors the production
// `mutationCreateGreetingUtterance` body in dsl/cognition/mutations.memql.
// Kept inline (rather than slicing the multi-construct file) so the test
// is hermetic and the exact insert shape under test is legible at a
// glance. The companion guard below is the contract: whatever the
// mutation inserts MUST validate against the v1:cognition:utterance
// concept schema. memql#419 regressed precisely because the insert wrote
// a `displayName` key that the concept (additionalProperties:false) does
// not allow, so the join greeting silently never landed.
const greetingMutationSource = `use cognition.concepts.{ utterance }

mutation utterance mutationCreateGreetingUtterance {
  args {
    spaceId        string  @required
    participantId  string  @required
    agentId        string  @required
    text           string  @required
    greetingKind   string  @required
  }
  insert {
    args.spaceId
    args.participantId
    participantType: "si"
    utteranceType: "text"
    args.text
    source: {
        kind: args.greetingKind,
        args.agentId
      }
  }
}`

// compileUtteranceSchema loads the REAL v1:cognition:utterance concept
// from the embedded DSL tree and returns its compiled definition schema,
// exactly the schema the production Concept.Create path validates against
// (see component/database/memory-nodes/concept_schema.go).
func compileUtteranceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	// Quiet loader so test output stays clean.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	concept, err := memoryNodes.DefaultRegistry().Get("v1:cognition:utterance")
	require.NoError(t, err, "v1:cognition:utterance concept must be loadable from the embedded DSL")

	raw, err := concept.DefinitionSchema()
	require.NoError(t, err)
	require.NotEmpty(t, raw, "utterance definition schema must be present")

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	require.NoError(t, compiler.AddResource("utterance.json", bytes.NewReader(raw)))
	schema, err := compiler.Compile("utterance.json")
	require.NoError(t, err)
	return schema
}

// renderGreetingPayload renders the greeting mutation into the concrete
// insert payload, the same JSON the engine would hand to
// Concept.Create.
func renderGreetingPayload(t *testing.T) map[string]any {
	t.Helper()

	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:utterance": {Name: "v1:cognition:utterance"},
	})
	fn, err := tryParseNewFunctionSyntax(
		"mutationCreateGreetingUtterance", "mutation",
		greetingMutationSource, "dsl/cognition/mutations.memql", registry,
	)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:cognition:utterance", fn.MutationTemplate.Concept)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"spaceId":       "v1:cognition:space:daily-2026-05-29",
		"participantId": "si-sofia-daily",
		"agentId":       "v1:agents:agent:assistant-znas",
		"text":          "Good to see you again -- jumping in.",
		"greetingKind":  "agentGreeting",
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	return payload
}

// TestGreetOnJoinUtterancePayloadValidates pins the fix for memql#419:
// the greet-on-join insert payload must validate against the real
// v1:cognition:utterance concept schema. Before the fix the payload
// carried a `displayName` field that the concept rejects.
func TestGreetOnJoinUtterancePayloadValidates(t *testing.T) {
	schema := compileUtteranceSchema(t)
	payload := renderGreetingPayload(t)

	// The greeting must NOT write displayName onto the utterance row.
	_, hasDisplayName := payload["displayName"]
	require.False(t, hasDisplayName,
		"greet-on-join must not write `displayName` onto the utterance (memql#419); "+
			"the SI name is carried by the participant referenced via participantId")

	// The rendered payload validates -- no additionalProperties rejection.
	require.NoError(t, schema.Validate(payload),
		"greet-on-join utterance payload must validate against v1:cognition:utterance")

	// Sanity: the fields the renderer actually emits are present.
	require.Equal(t, "si", payload["participantType"])
	require.Equal(t, "text", payload["utteranceType"])
	require.Equal(t, "Good to see you again -- jumping in.", payload["text"])
}

// TestUtteranceSchemaRejectsDisplayName proves the schema is the gate
// and `displayName` is genuinely not a valid utterance field -- i.e.
// the bug was a real schema violation, not an artifact of the loader.
// This is the exact failure mode reported in memql#419:
//
//	additionalProperties 'displayName' not allowed
func TestUtteranceSchemaRejectsDisplayName(t *testing.T) {
	schema := compileUtteranceSchema(t)
	payload := renderGreetingPayload(t)

	// Re-introduce the offending field and confirm the schema rejects it.
	payload["displayName"] = "Sofia"
	err := schema.Validate(payload)
	require.Error(t, err, "utterance schema must reject a stray displayName field")
	require.Contains(t, err.Error(), "displayName",
		"rejection must name the offending additionalProperty")
}
