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
			Capabilities           []map[string]any `json:"capabilities"`
			MatchedCapabilityCount int              `json:"matchedCapabilityCount"`
		}
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
		require.GreaterOrEqual(t, result.MatchedCapabilityCount, len(result.Capabilities))
		require.NotContains(t, string(nodes[0].Payload), `"total":`)
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

// TestWorkCapabilityAppNamesAnAppTheSeedsAndTheOSBothHave pins the namespace to
// OS-app map workCapabilityApp answers. A capability is offered only to a caller
// holding `read` on `app:<id>` (workCapabilityAllowed), and the action event it
// records tells the desktop to open the app of that id (the OS looks it up in
// the registry), so an id the seeds or the OS registry no longer has makes every
// construct in that namespace unusable to an agent and unnavigable on the
// desktop, with no error anywhere. The campaigns namespace is the one that
// moved: Campaigns became Email (memql#6033) and its constructs follow the app.
func TestWorkCapabilityAppNamesAnAppTheSeedsAndTheOSBothHave(t *testing.T) {
	_, seeded := seededAppResources(t)
	registered := map[string]bool{}
	for _, app := range navigationApps() {
		registered[app.ID] = true
	}
	if len(seeded) == 0 || len(registered) == 0 {
		t.Fatalf("read %d seeded apps and %d registered apps; the seeds or the navigation catalog moved", len(seeded), len(registered))
	}
	for _, tc := range []struct{ origin, name, want string }{
		{"campaigns/queries.memql", "campaigns", "email"},
		{"campaigns/mutations.memql", "createCampaign", "email"},
		{"accounts/queries.memql", "accounts", "accounts"},
		{"platform/queries.memql", "sitesAll", "deployables"},
		{"library/queries.memql", "artifacts", "files"},
		{"identity/queries.memql", "usersAll", "users"},
		{"identity/queries.memql", "myProfile", "identity"},
		{"groups/queries.memql", "groups", "users"},
		{"cluster/queries.memql", "nodes", "cluster"},
		{"worker/queries.memql", "workers", "fleet"},
		{"work/queries.memql", "goals", "nexus"},
		{"knowledge/queries.memql", "documents", "training"},
	} {
		got := workCapabilityApp(&Function{Name: tc.name, Origin: tc.origin})
		if got != tc.want {
			t.Errorf("%s (%s) belongs to app %q, want %q", tc.name, tc.origin, got, tc.want)
			continue
		}
		if _, ok := seeded["app:"+got]; !ok {
			t.Errorf("%s (%s) names app %q and the seeds declare no `read app:%s`; no role could ever use it", tc.name, tc.origin, got, got)
		}
		if !registered[got] {
			t.Errorf("%s (%s) names app %q and the OS navigation catalog has no such app; the desktop could never open it", tc.name, tc.origin, got)
		}
	}
}
