package memql

// fleet_call_sites_parse_test.go runs every statement the Fleet's Go callers
// build through the REAL function registry (epic memql#4349).
//
// WHY. The router's own tests use a fake FleetStore that returns fixture
// Candidates and never renders a query at all, and the forward's tests use a
// fake link. Both are the right shape for what they assert -- ordering,
// re-pick, the hop -- and both are exactly the shape
// component/grpc/render_query_args_parse_test.go was written about: a handler
// covered against a fake engine that records strings and parses nothing is a
// handler whose call sites can be wrong in production while every test stays
// green. Seven of them were, for two years.
//
// The two failures this catches are different, and only one of them is loud:
//
//   - A NAME THAT DOES NOT RESOLVE fails at execute, every time, the first
//     time a machine is dispatched to. Loud, but only in production -- nothing
//     in the unit suite parses these strings.
//   - AN ARGUMENT THE CONSTRUCT DOES NOT DECLARE is SILENTLY DISCARDED.
//     validateFunctionArgs iterates the DECLARED fields, and rejectUnknownArgs
//     is gated behind the MCP boundary, so a caller that invents a name gets no
//     error and no write. That is how `workersForOwnerWithStatus(ownerUserId:)`
//     would have kept "working" after the query lost its argument: same string,
//     no complaint, no filter.
//
// Keep this table in step with the callers. A call site that is not here is
// not covered.

import (
	"os"
	"regexp"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// fleetCallSites mirrors every engine call the Fleet's Go makes, with the
// argument names each one passes.
func fleetCallSites() []struct {
	fn    string
	args  []string
	where string
} {
	return []struct {
		fn    string
		args  []string
		where string
	}{
		// integrations/agent/worker/store.go -- the router's reads and its one
		// write. All three run under the session owner's actor; none takes an
		// owner argument, which is the point of memql#4351's collapse.
		{"myWorkersWithStatus", nil, "agent/worker/store.go WorkersForOwner"},
		{"routingPolicyForOwner", nil, "agent/worker/store.go RoutingPolicyForOwner"},
		{"touchWorkerSelected", []string{"registrationId"}, "agent/worker/store.go TouchWorkerSelected"},
		{"createWorkerInvocation", []string{
			"invocationId", "ownerUserId", "workerId", "agentId", "planId", "taskId",
			"correlationId", "tool", "action", "argsRedacted", "startedAt", "completedAt",
			"durationMs", "outcome", "exitCode", "signal", "errorCode", "errorMessage",
			"bytesIn", "bytesOut", "outputPreview", "routing",
		}, "agent/worker/store.go WriteInvocation"},

		// component/worker/store.go -- the registration lifecycle.
		{"workerByIdentityId", []string{"identityId"}, "component/worker/store.go WorkerByIdentityId"},
		{"workersForUser", []string{"ownerUserId"}, "component/worker/store.go WorkersForUser"},
		{"createWorkerRegistration", []string{
			"registrationId", "identityId", "name", "capabilities", "capabilityDescriptor",
			"labels", "concurrency", "platformInfo", "permissions", "version", "buildTag",
			"registeredAt", "lastSeenAt", "lastConnectedFromIP", "connectedNodeId",
		}, "component/worker/store.go CreateRegistration"},
		{"refreshWorkerRegistration", []string{
			"registrationId", "name", "capabilities", "capabilityDescriptor", "labels",
			"concurrency", "platformInfo", "permissions", "version", "buildTag",
			"lastSeenAt", "lastConnectedFromIP", "connectedNodeId",
		}, "component/worker/store.go RefreshRegistration"},
		{"updateWorkerLastSeen", []string{
			"registrationId", "lastSeenAt", "lastConnectedFromIP", "connectedNodeId", "activeCount",
		}, "component/worker/store.go UpdateLastSeen"},
		{"clearWorkerConnectedNode", []string{"registrationId"}, "component/worker/store.go ClearConnectedNode"},
		{"revokeWorker", []string{
			"registrationId", "revokedAt", "revokedBy", "revokeReason",
		}, "component/worker/store.go RevokeRegistration"},

		// The three reads these stores make that predate the Fleet. They are in
		// the table because TestFleetCallSitesCoverTheGoCallers reads the FILES,
		// not the epic: a table that listed only the new calls would report a
		// pass for a file whose older call sites it had never looked at, which
		// is the drift the coverage half exists to prevent.
		{"userByIdSystem", []string{"userId"}, "agent/worker/store.go UserPreferences"},
		{"agentAuthorizationsForSelf", nil, "agent/worker/store.go AgentAuthorization"},
		{"planById", []string{"planId"}, "agent/worker/store.go PlanScope + workbench/workspace_store.go"},

		// integrations/workbench/workspace_store.go -- the rows that were
		// declared and written by nothing until memql#4354.
		{"workspaceForPlan", []string{"planId"}, "workbench/workspace_store.go"},
		{"provisionWorkspace", []string{"workspaceId", "planId", "storageRoot", "nodeId"}, "workbench/workspace_store.go"},
		{"touchWorkspace", []string{"workspaceId"}, "workbench/workspace_store.go"},
		{"releaseWorkspace", []string{"workspaceId", "reason"}, "workbench/workspace_store.go"},
	}
}

func TestFleetCallSitesResolveAndDeclareTheirArguments(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this gate cannot see them and would "+
			"report a PASS on a call site whose target simply failed to parse:\n  %v",
			len(report.Skipped), report.Skipped)
	}

	sites := fleetCallSites()
	if len(sites) == 0 {
		t.Fatal("no call sites -- a gate over nothing passes for the wrong reason")
	}

	for _, site := range sites {
		fn, err := registry.Get(site.fn)
		if err != nil || fn == nil {
			t.Errorf("%s (%s): does not resolve in the function registry: %v.\n"+
				"This fails at EXECUTE the first time the path runs, and no unit test "+
				"parses these strings.", site.fn, site.where, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(site.args) == 0 && len(declared) > 0 {
			t.Errorf("%s (%s): the caller passes no arguments, but the construct declares %d. "+
				"A required field left unpassed is not refused here -- check the caller.",
				site.fn, site.where, len(declared))
			continue
		}
		for _, name := range site.args {
			if declared[name] {
				continue
			}
			t.Errorf("%s (%s): the caller passes %q, which the construct does not declare. "+
				"It is NOT refused -- rejectUnknownArgs is gated behind the MCP boundary -- so "+
				"the value is silently discarded and the row never receives it (memql#3626).",
				site.fn, site.where, name)
		}
	}
}

// TestFleetCallSitesCoverTheGoCallers is the other direction, and the one a
// hand-written table cannot supply on its own: it reads the THREE Go files
// that talk to the engine on the Fleet's behalf, extracts every construct name
// they render, and fails when one is not in the table above.
//
// Without it the table is an inventory that drifts the moment somebody adds a
// call, and a drifted inventory reports a pass for a call site it has never
// seen -- which is the same failure the table exists to prevent, one level up.
func TestFleetCallSitesCoverTheGoCallers(t *testing.T) {
	files := []string{
		"../../integrations/agent/worker/store.go",
		"../../component/worker/store.go",
		"../../integrations/workbench/workspace_store.go",
	}
	// `query name(` and `name(` inside a Go raw string or format string. The
	// leading backtick-or-quote is what keeps this off ordinary Go calls.
	pattern := regexp.MustCompile("[`\"](?:query |mutation )?([a-z][A-Za-z0-9]*)\\(")

	inTable := map[string]bool{}
	for _, site := range fleetCallSites() {
		inTable[site.fn] = true
	}

	found := 0
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v -- this gate cannot see a file it cannot open, and "+
				"silence would read as coverage", path, err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(body), -1) {
			name := m[1]
			// Only names the engine actually knows; the pattern also catches
			// Go-side helpers rendered into strings.
			if _, err := registryForFleetTest(t).Get(name); err != nil {
				continue
			}
			found++
			if !inTable[name] {
				t.Errorf("%s renders %q and the table in fleetCallSites() does not list it. "+
					"Add it with the arguments the caller passes -- an entry missing from the "+
					"table is a call site nothing checks.", path, name)
			}
		}
	}
	if found == 0 {
		t.Fatal("scanned three files and matched no engine construct -- the pattern has stopped " +
			"resolving, so this gate would now pass vacuously")
	}
}

// registryForFleetTest loads the tree once per test run.
var fleetTestRegistry *FunctionRegistry

func registryForFleetTest(t *testing.T) *FunctionRegistry {
	t.Helper()
	if fleetTestRegistry != nil {
		return fleetTestRegistry
	}
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), newLoadReport()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	fleetTestRegistry = registry
	return registry
}
