package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestResolveAttribute_TriggerOnSugarEmits5SegmentPattern guards against the
// same 4-segment topic regression that broke AI responses in the cognition
// integration. The @trigger(on=participant.created) sugar is not currently
// used by any shipped .memql file, but the resolver still handles it, and
// any pattern it emits must match the emitted-topic format
// graph.node.{action}.{partition}.{concept}.
//
// Before the fix, the resolver emitted graph.node.created.v1:cognition:<...>
// (4 segments, no partition segment), so an automation written with the
// sugar syntax would silently never fire against real CDC events.
//
// The automation is AUTHORED and parsed, not built by hand (memql#5359): the
// parser holds @trigger's keys to the annotation registry, so a hand-built
// FunctionDef would keep passing here while the parser refused `on=` before
// the resolver ever saw it.
func TestResolveAttribute_TriggerOnSugarEmits5SegmentPattern(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cognition:participant": {Name: "v1:cognition:participant"},
	})
	resolver := NewConceptResolver(registry)

	src := `use cognition.concepts.{ participant }

@trigger(on=participant.created)
automation testAutomation {
  step run {
    mutation createThing (id: "x")
  }
}`
	lowered, err := languageParser.NormaliseAll(src)
	require.NoError(t, err)
	file, err := languageParser.ParseFile(lowered)
	require.NoError(t, err, "the parser must accept the on= sugar the resolver implements")

	var funcDef *languageParser.FunctionDef
	for _, def := range file.Definitions {
		if fd, ok := def.(*languageParser.FunctionDef); ok && fd.Name == "testAutomation" {
			funcDef = fd
		}
	}
	require.NotNil(t, funcDef, "the automation must parse to a FunctionDef")

	require.NoError(t, resolver.ResolveFile(file, "v1"))

	var attr *languageParser.Attribute
	for _, a := range funcDef.Attributes {
		if a.Name == languageParser.AttrTrigger {
			attr = a
		}
	}
	require.NotNil(t, attr, "the automation must carry its @trigger")
	event, ok := attr.Args["event"]
	require.True(t, ok, "expected resolver to rewrite on= into event=")
	require.Equal(t, "graph.node.created.v1:cognition:participant", event,
		"sugar must emit the canonical 5-segment partition-aware pattern")
	_, stillHasOn := attr.Args["on"]
	require.False(t, stillHasOn, "resolver should remove the on= key after rewriting")
}
