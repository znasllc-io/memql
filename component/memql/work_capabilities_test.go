package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestCapabilityShortlistIncludesUsableContractsWithoutUnboundedContext(t *testing.T) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	e := &MemQLEngine{functions: newFunctionRegistry()}
	for _, fn := range []*Function{
		{Name: "availableRecords", FunctionKind: "query"},
		{Name: "filteredRecords", FunctionKind: "query", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "filter", Type: "object", Nested: []*FunctionArgsField{{Name: "status", Type: "string", Enum: []any{"open", "closed"}}}}}}},
		{Name: "largeRecords", FunctionKind: "query", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "criteria", Type: "string", Description: strings.Repeat("large contract ", 300)}}}},
		{Name: "hiddenRecords", FunctionKind: "query", ServerOnly: true},
		{Name: "secretRecords", FunctionKind: "mutation", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "secret", Type: "string", Secret: true}}}},
		{Name: "nestedSecretRecords", FunctionKind: "mutation", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "config", Type: "object", Nested: []*FunctionArgsField{{Name: "secret", Type: "string", Secret: true}}}}}},
	} {
		fn.Enabled = true
		fn.Origin = "records/queries.memql"
		require.NoError(t, e.functions.add(fn))
	}
	ctx := askTestActor()
	discover := func(search string) []map[string]any {
		t.Helper()
		nodes, err := e.workCapabilitiesBuiltin(ctx, map[string]any{"search": search}, 0)
		require.NoError(t, err)
		var result struct {
			Capabilities []map[string]any `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
		return result.Capabilities
	}
	shortlist := discover("records")
	require.Len(t, shortlist, 3)
	byName := map[string]map[string]any{}
	for _, match := range shortlist {
		byName[match["name"].(string)] = match
	}
	require.Equal(t, []any{}, byName["records.availableRecords"]["arguments"])
	args := byName["records.filteredRecords"]["arguments"].([]any)
	filter := args[0].(map[string]any)
	require.Equal(t, false, filter["optional"])
	require.Equal(t, []any{"status"}, filter["required"])
	require.Equal(t, []any{"open", "closed"}, filter["properties"].(map[string]any)["status"].(map[string]any)["enum"])
	require.NotContains(t, byName["records.largeRecords"], "arguments")
	require.Equal(t, true, byName["records.largeRecords"]["argumentsOmitted"])
	exact := discover("records.largeRecords")
	require.Len(t, exact, 1)
	require.Len(t, exact[0]["arguments"], 1)
	require.NotContains(t, exact[0], "argumentsOmitted")
	require.Empty(t, discover("records.hiddenRecords"))
	_, err := e.workCapabilitiesBuiltin(context.Background(), map[string]any{"search": "records"}, 0)
	require.Error(t, err)
}
