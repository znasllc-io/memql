package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
)

// Tests for the declared-must-be-used validator (Phase G.3.g pt 4).
// The validator walks every @use*(target) annotation and every
// args-block field on a function definition and asserts each one is
// exercised somewhere in the body. Stale declarations fail loud at
// load time.

func TestDeclaredUsage_AcceptsFullyUsedDecls(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
	// Every declaration is exercised.
	src := `@useConcept(space)
mutation testCreateSpace {
  args {
    name string @required
    description string
  }
  insert space {
    id: "x"
    name: args.name
    description: coalesce(args.description, "")
    status: "active"
  }
}`
	_, err := tryParseNewFunctionSyntax("testCreateSpace", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
}

func TestDeclaredUsage_RejectsStaleArgsField(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	// Mutation declares args.other but never uses it.
	src := `@useConcept(space)
mutation testMutation {
  args {
    name  string @required
    other string @required
  }
  insert space {
    id: "x"
    name: args.name
  }
}`
	_, err := tryParseNewFunctionSyntax("testMutation", "mutation", src, "test.memql", registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "other")
	require.Contains(t, err.Error(), "args.other")
}

func TestDeclaredUsage_RejectsStaleUseSpec(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	// Query declares @useSpec(specGhost) but never references it.
	src := `@useConcept(space)
@useSpec(specGhost)
query testQuery {
  args {
    userId string
  }
  filter ?.createdBy==args.userId
}`
	_, err := tryParseNewFunctionSyntax("testQuery", "query", src, "test.memql", registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "specGhost")
	require.Contains(t, err.Error(), "never referenced")
}
