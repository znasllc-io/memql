package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/stretchr/testify/require"
)

// classifyScalarOrExpr must map the bare reserved identifiers `now` and
// `timestamp` to their canonical call form so the template evaluator
// stamps a real timestamp at render time -- NOT the literal string
// "now". (memql#1629)
func TestClassifyScalarOrExpr_BareNowTimestamp(t *testing.T) {
	require.Equal(t, "now()", classifyScalarOrExpr("now"))
	require.Equal(t, "timestamp()", classifyScalarOrExpr("timestamp"))
	// Other bare identifiers / literals are unchanged.
	require.Equal(t, "args.foo", classifyScalarOrExpr("args.foo"))
	require.Equal(t, true, classifyScalarOrExpr("true"))
	require.Equal(t, nil, classifyScalarOrExpr("null"))
}

// An authored `createdAt: now` / `updatedAt: now` must render to a valid
// RFC3339 timestamp, not the literal string "now" that date-time
// concept validation rejects. (memql#1629)
func TestMutationTemplate_BareNowRendersTimestamp(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:notes:note": {Name: "v1:notes:note"},
	})
	src := `mutation note mutationCreateNoteProbe {
  args {
    noteId  string  @required
    body    string  @required
  }
  insert {
    id: args.noteId
    args.body
    updatedAt: now
    createdAt: now
  }
}`
	fn, err := tryParseNewFunctionSyntax("mutationCreateNoteProbe", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"noteId": "note-1",
		"body":   "hello",
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))

	updatedAt, ok := payload["updatedAt"].(string)
	require.True(t, ok, "updatedAt should be a string, got %T", payload["updatedAt"])
	require.NotEqual(t, "now", updatedAt, "bare `now` must not survive as the literal string")
	_, perr := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, perr, "updatedAt %q must be a valid RFC3339 timestamp", updatedAt)

	// `createdAt: now` is lifted out of the payload onto the node's
	// intrinsic CreatedAt; it must resolve to a real time, not fail
	// parsing the literal "now".
	require.NotNil(t, mutation.CreatedAt, "createdAt: now must resolve to a real timestamp on the node")
	require.False(t, mutation.CreatedAt.IsZero())
}

// A QUOTED "now" string literal must be preserved as the literal string
// -- only the bare keyword resolves to a timestamp. (memql#1629 guard
// against over-eager resolution.)
func TestMutationTemplate_QuotedNowStaysLiteral(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:notes:note": {Name: "v1:notes:note"},
	})
	src := `mutation note mutationQuotedNowProbe {
  args {
    noteId  string  @required
  }
  insert {
    id: args.noteId
    label: "now"
  }
}`
	fn, err := tryParseNewFunctionSyntax("mutationQuotedNowProbe", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"noteId": "note-2",
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	require.Equal(t, "now", payload["label"], "a quoted \"now\" must stay the literal string")
}
