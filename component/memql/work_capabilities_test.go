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

// work.workRetitleRun renames a goal because a person's message changed what it
// is about, and the Ask engine is its one caller. An agent that found it through
// capability discovery could rename a goal when nobody had said anything, so it
// is kept off the agent's tools the way createGoal is. The control is the same
// kind of function, from the same file, under the same caller: only the name
// differs.
func TestRetitleRunIsNotOfferedAsAnAgentCapability(t *testing.T) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	e := &MemQLEngine{functions: newFunctionRegistry()}
	offered := map[string]*Function{}
	for _, name := range []string{"workRetitleRun", "workRecordNote"} {
		fn := &Function{Name: name, FunctionKind: "builtin", Enabled: true, Origin: "records/builtins.memql"}
		require.NoError(t, e.functions.add(fn))
		offered[name] = fn
	}
	ctx := askTestActor()
	require.True(t, e.workCapabilityAllowed(ctx, offered["workRecordNote"]), "the control would be offered, so the exclusion is what hides the other")
	require.False(t, e.workCapabilityAllowed(ctx, offered["workRetitleRun"]))
	nodes, err := e.workCapabilitiesBuiltin(ctx, map[string]any{"search": "work"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "workRecordNote")
	require.NotContains(t, string(nodes[0].Payload), "workRetitleRun")
}

func TestCapabilitySearchRanksPurposeAboveIncidentalImplementationWords(t *testing.T) {
	for _, tc := range []struct{ search, name, purpose, incidental string }{
		{"library file read content", "library.libraryDocumentVersion", "Read the text content of a saved Library file.", "library.libraryFileBySha256"},
		{"read account records", "accounts.accountRecords", "Read the saved account records.", "accounts.archiveAccountRecords"},
		{"document generation", "documents.generateDocument", "Generate a document.", "documents.documentHistory"},
		{"archived customer records", "customers.archiveRecords", "Archive customer records.", "customers.createRecord"},
		{"scheduled appointment", "calendar.scheduleAppointment", "Schedule an appointment.", "calendar.readAppointment"},
		{"validate configuration", "settings.validation", "Validation of configuration.", "settings.configurationHistory"},
	} {
		actual := workCapabilityScore(tc.search, tc.name, tc.purpose, tc.purpose)
		incidental := workCapabilityScore(tc.search, tc.incidental, "Archive an item. Implementation notes mention "+tc.search, "Archive an item. Implementation notes mention "+tc.search)
		require.Greater(t, actual, incidental, tc.search)
	}
	require.Zero(t, workCapabilityScore("read", "platform.moduleReadiness", "Module readiness status."), "substring matches must not invent a read operation")
	require.Equal(t, workCapabilityScore("read files", "library.readFile", "Read a file."), workCapabilityScore("read file", "library.readFile", "Read a file."))
	require.Greater(t, workCapabilityScore("read the file", "library.readFile", "Read a file."), 0)
	require.Zero(t, workCapabilityScore("library.missingRead", "library.readFile", "Read a library file."))
	require.Zero(t, workCapabilityScore("records.archived", "records.archive", "Archive records."), "qualified identifiers must never be stemmed")
	require.Zero(t, workCapabilityScore("delete", "records.read", "Read saved records."), "normalization must not conflate different actions")
}

func TestSavedFileContentAndImageOperationsAreInTheRealCapabilityShortlist(t *testing.T) {
	// Discovery needs the shipped catalog and caller authority, not stored rows.
	e := newQuietEngine(t)
	t.Cleanup(e.cache.close)
	require.NoError(t, e.Init(loadedConceptRegistry(t)))
	for _, name := range []string{"fleetEnergySettings", "routerSetApiKey", "platform.sourceCredentialCreate", "integrations.integrationConfigure"} {
		private, ok := e.lookupBuiltinFunction(name)
		require.True(t, ok, name)
		require.False(t, e.workCapabilityAllowed(askTestActor(), private), "private input must stay out of discovery: %s", name)
		secret := false
		for _, field := range workCapabilityFields(private) {
			secret = secret || workCapabilitySecret(field)
		}
		require.True(t, secret, "private inputs must be declared, not hidden only by another gate: %s", name)
		_, err := e.workExecuteBuiltin(askTestActor(), map[string]any{"name": private.Name, "arguments": map[string]any{}}, 0)
		require.ErrorContains(t, err, "capability is unavailable for this person", name)
	}
	fn, lookupErr := e.functions.Get("compose.composeMaterialize")
	require.NoError(t, lookupErr)
	require.Contains(t, fn.Description, "Create a saved document file")
	require.True(t, e.workCapabilityAllowed(askTestActor(), fn), "document materialization remains discoverable")
	for _, tc := range []struct{ search, name string }{
		{"library file read content", "library.libraryReadFile"},
		{"Read file to identify first bird", "library.libraryReadFile"},
		{"What is the number one bird in the markdown file you just saved? Read that file to identify it.", "library.libraryReadFile"},
		{"generate image", "library.libraryGenerateImage"},
		{"create image file", "library.libraryGenerateImage"},
		{"image generation create", "library.libraryGenerateImage"},
		{"imageGen generate image model", "library.libraryGenerateImage"},
		{"create files", "compose.composeMaterialize"},
		{"Can you create files and generate images using the capabilities available in this MemQL system?", "compose.composeMaterialize"},
	} {
		rows, err := e.workCapabilitiesBuiltin(askTestActor(), map[string]any{"search": tc.search}, 0)
		require.NoError(t, err)
		var result struct {
			Capabilities []struct {
				Name string `json:"name"`
			} `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
		names := []string{}
		for _, entry := range result.Capabilities {
			names = append(names, entry.Name)
		}
		require.Contains(t, names, tc.name, tc.search)
	}
}

func TestSoftwareSourceDiscoveryFindsExistingReadContracts(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	caller := rankActorCtx("software-source-discovery", auth.RoleDeveloper)
	for _, tc := range []struct{ search, name string }{
		{"connected repositories", "platform.packagesAll"},
		{"repository pipelines", "pipelines.pipelinesForOwner"},
		{"source connections", "platform.sourceConnectionsMine"},
	} {
		t.Run(tc.search, func(t *testing.T) {
			rows, err := e.workCapabilitiesBuiltin(caller, map[string]any{"search": tc.search}, 0)
			require.NoError(t, err)
			var response struct{ Capabilities []struct{ Name string } }
			require.NoError(t, json.Unmarshal(rows[0].Payload, &response))
			names := []string{}
			for _, capability := range response.Capabilities {
				names = append(names, capability.Name)
			}
			require.Contains(t, names, tc.name)
		})
	}
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
		{"pipelines/queries.memql", "pipelinesForOwner", "pipelines"},
		{"repositoryauthority/builtins.memql", "repositoryChangeAccess", "pipelines"},
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

func TestSourceCapabilityKeepsItsPipelinesDestination(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	fn, err := e.functions.Get("platform.sourceConnectionsMine")
	require.NoError(t, err)
	require.Equal(t, "pipelines", workCapabilityApp(fn))
	require.True(t, e.workCapabilityAllowed(rankActorCtx("source-developer", auth.RoleDeveloper), fn))
}
