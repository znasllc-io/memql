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
func TestResolveAttribute_TriggerOnSugarEmits5SegmentPattern(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cognition:participant": {Name: "v1:cognition:participant"},
	})
	resolver := NewConceptResolver(registry)

	funcDef := &languageParser.FunctionDef{
		Name: "testAutomation",
		Type: languageParser.FunctionTypeAutomation,
		Attributes: []*languageParser.Attribute{
			{
				Name: languageParser.AttrTrigger,
				Args: map[string]any{
					"on": "participant.created",
				},
			},
		},
	}

	file := &languageParser.File{
		Uses: []*languageParser.UseDeclaration{
			{Path: "cognition.participant", Parts: []string{"cognition", "participant"}},
		},
		Definitions: []languageParser.Node{funcDef},
	}

	require.NoError(t, resolver.ResolveFile(file, "v1"))

	attr := funcDef.Attributes[0]
	event, ok := attr.Args["event"]
	require.True(t, ok, "expected resolver to rewrite on= into event=")
	require.Equal(t, "graph.node.created.v1:cognition:participant", event,
		"sugar must emit the canonical 5-segment partition-aware pattern")
	_, stillHasOn := attr.Args["on"]
	require.False(t, stillHasOn, "resolver should remove the on= key after rewriting")
}
