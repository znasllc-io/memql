package memql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// automation_graph_read_test.go -- the two reads behind MemQL OS's
// Cluster > Automations section (epic memql#5380, task memql#5384): the
// capability both builtins declare, the graph rows projected from the
// scheduler's source, and the loop stops read as the cluster and projected
// with no payload.

// fakeGraphSource is an AutomationGraphSource answering fixed rows.
type fakeGraphSource struct {
	rows []map[string]any
	err  error
}

func (f fakeGraphSource) AutomationGraphRows() ([]map[string]any, error) { return f.rows, f.err }

// treeBuiltinEngine is an engine holding the embedded tree's builtins, so a
// builtin call meets the @requiresCapability the tree declares on it -- not
// one a test wrote down.
func treeBuiltinEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	fns := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.New(slog.NewTextHandler(io.Discard, nil)), fns); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	return &MemQLEngine{functions: fns}
}

func builtinCall(name string) *BuiltinFunctionExpression {
	return &BuiltinFunctionExpression{Name: name, Executor: name}
}

// A refused run's stored row as the shaped read hands it back, carrying every
// payload field a run row has -- the fields the projection must NOT pass on.
func storedStopRow() map[string]any {
	return map[string]any{
		"id":             "v1:work:run:7d1f0c2a-0000-4000-8000-000000000001",
		"createdAt":      "2026-09-14T16:02:11.000000001Z",
		"automationName": "advanceTicket",
		"errorCode":      "loop_depth_exceeded",
		"errorMessage":   "loop_depth_exceeded: advanceTicket would run at depth 17, past the cap of 16; chain: ...",
		"finishedAt":     "2026-09-14T16:02:11Z",
		"input":          map[string]any{"secret": "an argument value"},
		"variables":      map[string]any{"token": "a bound variable"},
		"triggerEvent":   map[string]any{"topic": "graph.node.updated.v1:t:ticket", "payload": map[string]any{"body": "a row"}},
		"outcome": map[string]any{
			"executorStatus": "failed",
			"loop": map[string]any{
				"reason":        "depth",
				"depth":         float64(17),
				"cap":           float64(16),
				"correlationId": "evt-abc",
				"chain": []any{
					map[string]any{"automation": "routeTicket", "runId": "run-1"},
					map[string]any{"automation": "advanceTicket", "runId": "run-2"},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// The capability: read on app:cluster/automations, on both builtins
// ---------------------------------------------------------------------------

func TestTheAutomationReadsRequireTheAutomationsCapability(t *testing.T) {
	installSeededCatalog(t)
	e := treeBuiltinEngine(t)
	e.SetAutomationGraphSource(fakeGraphSource{rows: []map[string]any{{"name": "routeRequest", "stratum": 0}}})
	e.loopStopRows = func(context.Context) ([]map[string]any, error) {
		return []map[string]any{storedStopRow()}, nil
	}

	for _, name := range []string{BuiltinExecutorAutomationGraph, BuiltinExecutorAutomationLoopStops} {
		fn, err := e.functions.Get(name)
		if err != nil || fn == nil {
			t.Fatalf("%s is not in the loaded tree: %v", name, err)
		}
		// THE CONTROL COMES FROM THE TREE: without it, a builtin that lost its
		// annotation would be admitted for everybody below and this test
		// would call that a pass.
		if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbRead, Resource: "app:cluster/automations"}) {
			t.Fatalf("%s declares %+v, want read on app:cluster/automations", name, fn.RequiresCapability)
		}

		for _, role := range []string{"user", "viewer"} {
			nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), builtinCall(name), 0)
			if err == nil {
				t.Fatalf("%s answered a %s with %d rows: the wiring of every automation is the operator's, not every signed-in person's", name, role, len(nodes))
			}
			// The code leads the sentence, which is what the OS reads; a
			// refusal for another reason would pass a looser check.
			if !strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": ") || !strings.Contains(err.Error(), "app:cluster/automations") {
				t.Fatalf("%s refused a %s with %q, want the capability refusal naming app:cluster/automations", name, role, err)
			}
		}

		// THE REACHABLE POSITIVE: every role the seeds grant it is admitted,
		// and gets rows -- a gate that refused everybody would pass the half
		// above.
		for _, role := range []string{"owner", "developer", "admin"} {
			nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), builtinCall(name), 0)
			if err != nil {
				t.Fatalf("%s refused a %s, whom the seeds grant it: %v", name, role, err)
			}
			if len(nodes) != 1 {
				t.Fatalf("%s answered a %s with %d rows, want 1", name, role, len(nodes))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The graph
// ---------------------------------------------------------------------------

func TestTheAutomationGraphProjectsTheSchedulersRows(t *testing.T) {
	e := &MemQLEngine{}
	rows := []map[string]any{
		{"name": "recordTransition", "stratum": 1, "writes": []string{"v1:forge:requestEvent"}},
		{"name": "routeRequest", "stratum": 0, "trigger": "graph.node.created.v1:forge:request",
			"edgesOut": []map[string]any{{"to": "recordTransition", "concept": "v1:forge:request", "decided": true}}},
		{"name": ""},
	}
	e.SetAutomationGraphSource(fakeGraphSource{rows: rows})

	nodes, err := e.evaluateAutomationGraphExpression(ownerRoleCtx("owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	// A row with no name has no id: a builtin's reply is one id-keyed map, and
	// a blank key would collapse with the next blank one.
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want the two named rows", len(nodes))
	}
	if nodes[0].ID != "recordTransition" || nodes[1].ID != "routeRequest" {
		t.Errorf("ids = %s, %s; want the automation names, sorted", nodes[0].ID, nodes[1].ID)
	}
	for i, n := range nodes {
		if n.Concept != AutomationNodeConcept {
			t.Errorf("node %s concept = %q, want %q", n.ID, n.Concept, AutomationNodeConcept)
		}
		var got map[string]any
		if err := json.Unmarshal(n.Payload, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{}
		raw, _ := json.Marshal(rows[i])
		_ = json.Unmarshal(raw, &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("node %s payload = %v, want the row as the source gave it: %v", n.ID, got, want)
		}
	}
}

func TestTheAutomationGraphWithNoSchedulerIsAnErrorNotAnEmptyGraph(t *testing.T) {
	e := &MemQLEngine{}
	nodes, err := e.evaluateAutomationGraphExpression(ownerRoleCtx("owner-1"))
	// An empty list is an answer -- "this cluster loaded no automations" --
	// and a node with no scheduler cannot give it.
	if err == nil {
		t.Fatalf("answered %d rows with no scheduler wired", len(nodes))
	}
	if !strings.Contains(err.Error(), "scheduler") {
		t.Errorf("refusal %q does not say what is missing", err)
	}

	e.SetAutomationGraphSource(fakeGraphSource{err: errors.New("automation graph: not registered yet")})
	if _, err := e.evaluateAutomationGraphExpression(ownerRoleCtx("owner-1")); err == nil || !strings.Contains(err.Error(), "not registered yet") {
		t.Errorf("the source's refusal did not come through in its own words: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The loop stops
// ---------------------------------------------------------------------------

// loopStopFields is the projection's whole vocabulary.
var loopStopFields = map[string]bool{
	"runId": true, "automationName": true, "finishedAt": true, "depth": true,
	"cap": true, "reason": true, "correlationId": true, "chain": true,
}

func TestALoopStopCarriesNoPayload(t *testing.T) {
	node, ok := loopStopNode(storedStopRow())
	if !ok {
		t.Fatal("the stored stop was dropped")
	}
	var got map[string]any
	if err := json.Unmarshal(node.Payload, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
		if !loopStopFields[k] {
			t.Errorf("the stop carries %q: a run row's input, variables and triggering event must not reach this reply", k)
		}
	}
	sort.Strings(keys)
	if len(keys) != len(loopStopFields) {
		t.Errorf("keys = %v, want exactly the eight the projection names", keys)
	}
	for _, leaked := range []string{"an argument value", "a bound variable", "a row", "would run at depth"} {
		if raw := string(node.Payload); strings.Contains(raw, leaked) {
			t.Errorf("the payload leaks %q: %s", leaked, raw)
		}
	}

	if node.ID != "7d1f0c2a-0000-4000-8000-000000000001" || got["runId"] != node.ID {
		t.Errorf("id = %q, runId = %v; want the run's bare id on both", node.ID, got["runId"])
	}
	if node.Concept != AutomationLoopStopConcept {
		t.Errorf("concept = %q, want %q -- a v1:work:run node would be filtered out for every reader who is not a cluster owner", node.Concept, AutomationLoopStopConcept)
	}
	if got["automationName"] != "advanceTicket" || got["reason"] != "depth" || got["depth"] != float64(17) || got["cap"] != float64(16) || got["correlationId"] != "evt-abc" {
		t.Errorf("stop = %v", got)
	}
	chain, _ := got["chain"].([]any)
	if len(chain) != 2 || !reflect.DeepEqual(chain[0], map[string]any{"automation": "routeTicket", "runId": "run-1"}) {
		t.Errorf("chain = %v, want the two prior runs, oldest first", got["chain"])
	}
	// The SDK orders a builtin's nodes by createdAt, newest first, so the
	// stop is stamped with when it happened.
	if want, _ := time.Parse(time.RFC3339, "2026-09-14T16:02:11Z"); !node.CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want the finishedAt %v", node.CreatedAt, want)
	}
}

// A parent run whose sub-automation was refused lands terminal with the code
// and no loop record of its own. Its depth and cap are ABSENT: a zero would
// read as a chain stopped at depth 0, which never happens, and the page draws
// an em dash for an absent figure.
func TestAStopWithNoLoopRecordLeavesDepthAndCapAbsent(t *testing.T) {
	row := storedStopRow()
	row["outcome"] = map[string]any{"executorStatus": "failed"}
	node, ok := loopStopNode(row)
	if !ok {
		t.Fatal("the stored stop was dropped")
	}
	var got map[string]any
	if err := json.Unmarshal(node.Payload, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"depth", "cap", "reason", "correlationId"} {
		if v, present := got[key]; present {
			t.Errorf("%s = %v, want it absent: this run carries no loop record", key, v)
		}
	}
	if raw, _ := json.Marshal(got["chain"]); string(raw) != "[]" {
		t.Errorf("chain = %s, want []", raw)
	}
	if got["automationName"] != "advanceTicket" {
		t.Errorf("automationName = %v", got["automationName"])
	}
}

func TestALoopStopWithNoIdIsDropped(t *testing.T) {
	row := storedStopRow()
	delete(row, "id")
	if _, ok := loopStopNode(row); ok {
		t.Error("a stop with no run id was kept; it would collapse with the next one in the id-keyed reply")
	}
}

// THE STOPS ARE READ AS THE CLUSTER, NOT AS THE CALLER. The context replaces
// the caller's actor with a synthetic, unranked cluster owner and stamps
// internal origin, and the query it reaches takes no argument -- so a caller
// can ask for the read and cannot shape it. This is the precondition that
// earns component/memql's request-derived stamp in the root allowlist.
func TestTheLoopStopsReadAsTheClusterNotTheCaller(t *testing.T) {
	caller := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:somebody",
		Role:   auth.Role("admin"),
	})
	ctx := loopStopsReadContext(caller)

	if !auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("the stops read is not stamped internal; the @serverOnly query would refuse it on every call")
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok {
		t.Fatal("the read carries no actor")
	}
	if ac.UserId == "v1:identity:user:somebody" || !strings.HasPrefix(ac.UserId, "system:maintenance:") {
		t.Errorf("actor = %q: the caller's identity survived into the read", ac.UserId)
	}
	if !ac.IsClusterOwner() || !ac.Unranked || !ac.Synthetic {
		t.Errorf("actor = %+v, want a synthetic, unranked cluster owner: under anything narrower the composite tier answers zero rows and no error", ac)
	}
}

func TestTheStopsQueryIsServerOnlyAndTakesNoCallerArgument(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fns := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(slog.New(slog.NewTextHandler(io.Discard, nil)), fns, memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	fn, err := fns.Get("workRunsStoppedByLoops")
	if err != nil || fn == nil {
		t.Fatalf("workRunsStoppedByLoops is not in the loaded tree: %v", err)
	}
	if !fn.ServerOnly {
		t.Error("workRunsStoppedByLoops is not @serverOnly: a client could read every stopped chain in the cluster")
	}
	if fn.ArgsSchema != nil && len(fn.ArgsSchema.Fields) > 0 {
		t.Errorf("workRunsStoppedByLoops declares %d args: the read runs under internal origin, so it must take nothing a caller could supply", len(fn.ArgsSchema.Fields))
	}
	if loopStopsQuery != "query workRunsStoppedByLoops()" {
		t.Errorf("the Go call is %q; it must name the query and pass nothing", loopStopsQuery)
	}
}
