package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

// A runtime bundle may still declare its own tools with these short names.
// Exercise the actual engine registry, schema projection and execution lookup:
// a stub containing only one namespace cannot reproduce the production loss.
func TestHarnessFleetToolsSurviveBundleNameCollisions(t *testing.T) {
	engine := procedureTestEngine(t)
	adapter := &CognitionEngineAdapter{Engine: engine}
	names := []string{"workerStatus", "requestComputerUseScope", "workerHost", "workerComputer", "workbenchHost"}
	for _, name := range names {
		require.NoError(t, engine.Tools().Upsert(&memql.Tool{
			Name: name, Origin: "testproduct/tools.memql", Description: "Product duplicate must not replace the harness tool",
			InputSchema: []byte(`{"type":"object","properties":{}}`),
			Handler:     &memql.ToolHandler{Type: "function", FunctionName: "productDuplicateMustNotRun"},
		}))
		_, err := engine.Tools().Get(name)
		require.ErrorContains(t, err, "declared in 2 namespaces", "ordinary registry lookups must remain unambiguous")
	}
	definitions := adapter.ToolDefinitionsForNames(names)
	require.Len(t, definitions, len(names), "the supplied tool set must retain the permission gate and both execution surfaces")
	for n, definition := range definitions {
		require.Equal(t, names[n], definition.Name, "provider-facing names and continuation hooks remain stable")
		require.NotContains(t, definition.Description, "Product duplicate")
		_, callee, ok := adapter.ToolCallee(definition.Name)
		require.True(t, ok, "recording must resolve the same definition as execution")
		require.NotEqual(t, "productDuplicateMustNotRun", callee)
		// No authenticated agent: resolution must reach the actual tool's
		// authorization/argument gate, never a product duplicate or ambiguity.
		_, err := adapter.ExecuteToolByName(context.Background(), definition.Name, map[string]any{})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "declared in 2 namespaces")
		require.NotContains(t, err.Error(), "productDuplicateMustNotRun")
	}
	_, _, ok := adapter.ToolCallee("testproduct.workerHost")
	require.False(t, ok, "explicit qualification must not be redirected")
}
