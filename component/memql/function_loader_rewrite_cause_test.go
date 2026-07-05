package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestNamedInsertFormSurfacesRewriteCause pins memql#1055: a struct-form
// mutation that uses the RETIRED named-write form `insert <Concept> { ... }`
// (instead of the canonical bare `insert { ... }`) must fail to load with
// an error that NAMES the real cause -- the retired named-write form --
// rather than the cryptic legacy-parser "unexpected token mutation".
//
// Before the fix the rewrite error was swallowed in tryParseNewFunctionSyntax
// and the un-normalised struct source fell through to the legacy parser,
// which rejected the leading `mutation` keyword with a misleading token
// complaint. The product pack's mutations.memql tripped exactly this:
// ~22 slices were skipped at load with "unexpected token mutation", masking
// that the bodies used the retired `insert canvasState { ... }` shape.
func TestNamedInsertFormSurfacesRewriteCause(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:exampleapp:canvasState": {Name: "v1:exampleapp:canvasState"},
	})

	// Named-write form -- retired (#988). The bare `insert { ... }` form is
	// canonical; restating the concept after `insert` must be rejected.
	src := `use exampleapp.concepts.{ canvasState }

mutate canvasState mutationCreateCanvasState {
  args {
    stateId  string  @required
    kind     string  @required
  }
  insert canvasState {
    id: args.stateId
    args.kind
  }
}`

	_, err := tryParseNewFunctionSyntax(
		"mutationCreateCanvasState", "mutation",
		src, "exampleapp/mutations.memql", registry,
	)
	require.Error(t, err, "named-write form must fail to load")

	// The actionable cause -- the retired named-write form -- must be in the
	// error, not just the generic parser token complaint.
	msg := err.Error()
	require.Contains(t, msg, "retired",
		"error must surface the retired named-write rewrite cause; got: %s", msg)
	require.True(t, strings.Contains(msg, "insert"),
		"error must name the offending `insert` write block; got: %s", msg)
}

// TestBareInsertFormLoadsClean is the companion: the canonical bare
// `insert { ... }` form (the target of the #1055 migration) loads without
// error, proving the fix is "migrate the named form to bare", not "accept
// the named form again".
func TestBareInsertFormLoadsClean(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:exampleapp:canvasState": {Name: "v1:exampleapp:canvasState"},
	})

	src := `use exampleapp.concepts.{ canvasState }

mutate canvasState mutationCreateCanvasState {
  args {
    stateId  string  @required
    kind     string  @required
  }
  insert {
    id: args.stateId
    args.kind
  }
}`

	fn, err := tryParseNewFunctionSyntax(
		"mutationCreateCanvasState", "mutation",
		src, "exampleapp/mutations.memql", registry,
	)
	require.NoError(t, err, "bare insert form must load clean")
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:exampleapp:canvasState", fn.MutationTemplate.Concept)
}
