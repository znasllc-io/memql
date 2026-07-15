package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// These tests pin the fylo#63 @createOnly engine primitive:
// dropCreateOnlyFields removes the named fields from a partial (delta)
// payload on the read-merge path (an existing prior row), BEFORE the
// append/merge, so the stored value survives instead of being clobbered.
// This is what makes a deterministic-id re-stage genuinely idempotent for
// lifecycle fields another writer owns after creation (stageOutboundRequest
// seeds status/attempts at birth but must not reset a row the outbound
// worker has since moved to sending/sent/failed).
//
// Pure-Go engine path: identical on whichever node runs the write; no
// cross-node state involved.

// Re-stage onto an existing row: the create-only fields are dropped from
// the delta, so the prior (worker-owned) values survive the merge while
// the refreshed content fields still overwrite.
func TestDropCreateOnlyFields_PreservesPriorLifecycleState(t *testing.T) {
	// The row the outbound worker has already delivered.
	prior := map[string]any{
		"status":   "sent",
		"attempts": float64(1),
		"body":     "old body",
		"target":   "https://hook.example/a",
	}
	// A re-stage delta: the mutation re-writes status=pending, attempts=0
	// plus fresh content.
	delta := map[string]any{
		"status":   "pending",
		"attempts": float64(0),
		"body":     "new body",
		"target":   "https://hook.example/a",
	}

	dropCreateOnlyFields(delta, []string{"status", "attempts"})
	mergePayloadFields(prior, delta, nil)

	require.Equal(t, "sent", prior["status"], "@createOnly status must not be reset by a re-stage")
	require.Equal(t, float64(1), prior["attempts"], "@createOnly attempts must not be reset by a re-stage")
	require.Equal(t, "new body", prior["body"], "non-createOnly content still refreshes on re-stage")
}

// Without @createOnly (empty list) the default top-level-replace contract
// applies -- the delta's status/attempts overwrite the stored values. This
// is the pre-fix (bug) behavior, pinned so the annotation's effect is
// unambiguous.
func TestDropCreateOnlyFields_DefaultReplaceWhenNotListed(t *testing.T) {
	prior := map[string]any{"status": "sent", "attempts": float64(1)}
	delta := map[string]any{"status": "pending", "attempts": float64(0)}

	dropCreateOnlyFields(delta, nil) // not opted in
	mergePayloadFields(prior, delta, nil)

	require.Equal(t, "pending", prior["status"], "without @createOnly, status replaces wholesale")
	require.Equal(t, float64(0), prior["attempts"])
}

// Only the named fields are dropped; a create-only list must not touch
// sibling delta keys. Blank/whitespace entries are ignored.
func TestDropCreateOnlyFields_OnlyNamedFields(t *testing.T) {
	delta := map[string]any{"status": "pending", "attempts": float64(0), "body": "x"}

	dropCreateOnlyFields(delta, []string{"status", "  ", "attempts"})

	require.NotContains(t, delta, "status")
	require.NotContains(t, delta, "attempts")
	require.Equal(t, "x", delta["body"], "non-createOnly field survives the drop")
}

// TestMutationTemplate_CreateOnlyAnnotationPlumbing parses an inline
// @createOnly insert mutation through the same loader path the unified
// tree uses and asserts the annotation lands on both the template and the
// rendered MutationNode (the value executeWrite consults).
func TestMutationTemplate_CreateOnlyAnnotationPlumbing(t *testing.T) {
	registry := newMemoryRegistry(map[string]*concept.Concept{
		"v1:platform:outboundRequest": {Name: "v1:platform:outboundRequest"},
	})

	src := `@enabled
@createOnly("status", "attempts")
@description("Stage an outbound delivery, idempotent by requestId.")
mutate outboundRequest stageOutboundRequest {
  args {
    requestId  string  @required
    body       string  @required
  }
  insert {
    id: args.requestId
    args.body
    status: "pending"
    attempts: 0
  }
}`

	fn, err := tryParseNewFunctionSyntax("stageOutboundRequest", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, []string{"status", "attempts"}, fn.MutationTemplate.CreateOnlyFields)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"requestId": "req-001",
		"body":      "hello",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"status", "attempts"}, mutation.CreateOnlyFields)
}

// A single-field @createOnly parses to a one-element list (attr.Value is a
// bare string, not a []string, in that form).
func TestMutationTemplate_CreateOnlySingleField(t *testing.T) {
	registry := newMemoryRegistry(map[string]*concept.Concept{
		"v1:platform:outboundRequest": {Name: "v1:platform:outboundRequest"},
	})

	src := `@enabled
@createOnly("status")
mutate outboundRequest stageOne {
  args {
    requestId  string  @required
  }
  insert {
    id: args.requestId
    status: "pending"
  }
}`

	fn, err := tryParseNewFunctionSyntax("stageOne", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, []string{"status"}, fn.MutationTemplate.CreateOnlyFields)
}

// TestMutationTemplate_CreateOnlyRejectedOnUpdate pins the load-time gate:
// @createOnly on an update-kind mutation is a hard error. An update always
// targets an existing row, so a create-only field would ALWAYS be dropped
// and could never be written -- a silent footgun.
func TestMutationTemplate_CreateOnlyRejectedOnUpdate(t *testing.T) {
	registry := newMemoryRegistry(map[string]*concept.Concept{
		"v1:platform:outboundRequest": {Name: "v1:platform:outboundRequest"},
	})

	src := `@enabled
@createOnly("status")
mutate outboundRequest updateBad {
  args {
    requestId  string  @required
  }
  update {
    id: args.requestId
    status: "pending"
  }
}`

	_, err := tryParseNewFunctionSyntax("updateBad", "mutation", src, "test.memql", registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "@createOnly is only valid on insert mutations")
}
