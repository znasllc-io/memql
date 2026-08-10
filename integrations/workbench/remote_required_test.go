package workbench

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// remote_required_test.go is the coverage for memql#3506.
//
// MEMQL_WORKBENCH_REMOTE=1 is an operator asserting THIS WORK DOES NOT RUN ON
// THE AGENT. The integration used to read a missing workbench peer as "dispatch
// locally", which does not honour that assertion -- it inverts it, and does so
// most readily in the case that matters, which is the workbench being
// unavailable. A sandbox that silently becomes not-a-sandbox is worth less than
// an error.
//
// That fallback is also what made memql#3450 invisible for its whole life. The
// shipped agent.yaml sets both MEMQL_WORKBENCH_REMOTE=1 and the peer seed; the
// seed was dropped at parse time, so the router had no peer, so every workbench
// tool call ran on the agent pod. No error, no warning, correct-looking
// results. #3450 fixed the dropped seed; these tests are about the silence.

// decodeDispatch reads the single dispatchResult node a workbench call returns.
func decodeDispatch(t *testing.T, nodes []memorynodes.MemoryNode) dispatchResult {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("want exactly one dispatch node, got %d", len(nodes))
	}
	var res dispatchResult
	if err := json.Unmarshal(nodes[0].Payload, &res); err != nil {
		t.Fatalf("decoding the dispatch payload: %v", err)
	}
	return res
}

// remoteIntegration builds an agent-side integration in the shipped cluster
// posture: remote mode requested, and NO workbench peer reachable. The router
// is nil, which is the state a router with no healthy peer is
// indistinguishable from at this seam -- Forward returns ErrNoWorkbenchPeer in
// both cases, and both are covered below.
func remoteIntegration(t *testing.T, root string) *Integration {
	t.Helper()
	t.Setenv(rootEnvVar, root)
	i := NewIntegration(nil)
	i.remote = true
	return i
}

func execArgs(planId string) map[string]any {
	return map[string]any{
		"planId": planId,
		"action": "exec",
		"args":   map[string]any{"cmd": "true"},
	}
}

// TestRemoteModeWithNoPeerFailsInsteadOfRunningLocally is the issue's first
// acceptance line, and the whole point of the change.
//
// The assertion that carries the weight is not the error -- it is the EMPTY
// WORKSPACE ROOT afterwards. An error beside a directory the agent quietly
// provisioned and executed in would be the same bug with better logging.
func TestRemoteModeWithNoPeerFailsInsteadOfRunningLocally(t *testing.T) {
	root := t.TempDir()
	i := remoteIntegration(t, root)

	nodes, err := i.handleDispatchHost(context.Background(), execArgs("v1:planner:plan:p1"), 0)
	if err != nil {
		t.Fatalf("the call must fail as a structured tool result, not a Go error the tool loop crashes on: %v", err)
	}
	res := decodeDispatch(t, nodes)
	if res.OK {
		t.Fatal("a workbench call with remote mode requested and no reachable peer came back ok=true; " +
			"the operator asked for isolation and got local execution")
	}
	if res.ErrorCode != "no_workbench_peer" {
		t.Errorf("errorCode = %q, want no_workbench_peer", res.ErrorCode)
	}

	// The load-bearing assertion: nothing ran here.
	entries, rerr := os.ReadDir(root)
	if rerr != nil {
		t.Fatalf("reading the workspace root: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused remote dispatch provisioned a local workspace (%v); the refusal has to happen "+
			"BEFORE provisionForPlan, or the sandbox boundary moved while the error said it did not",
			entries)
	}
}

// TestTheRefusalNamesThePeerAndTheEnvVarThatConfiguresIt is the issue's second
// acceptance line.
//
// It is not decoration. The failure this replaces was undiagnosable in the
// direction that mattered -- an operator reading "workbench unavailable" has no
// way to know whether they are looking at a crashed pod or at a seed their
// config never delivered, and #3450 was precisely the second. The message has
// to name what is missing AND where it is configured.
func TestTheRefusalNamesThePeerAndTheEnvVarThatConfiguresIt(t *testing.T) {
	i := remoteIntegration(t, t.TempDir())

	nodes, err := i.handleDispatchHost(context.Background(), execArgs("v1:planner:plan:p1"), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	msg := decodeDispatch(t, nodes).ErrorMsg

	for _, want := range []string{
		"workbench",                      // which peer type is missing
		"MEMQL_WORKER_PEERS",             // where the peer address is configured
		"MEMQL_WORKBENCH_REMOTE",         // the assertion being honoured
		"MEMQL_WORKBENCH_LOCAL_FALLBACK", // the way out, if local really is wanted
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not name %q -- an operator cannot act on it.\nmessage: %s", want, msg)
		}
	}
}

// TestLocalFallbackIsReachableOnlyByExplicitOptIn is the issue's third
// acceptance line: the two intentions -- "run this remotely" and "run it here
// if you must" -- have to be distinguishable, which means the second cannot be
// spelled as the ABSENCE of configuration.
func TestLocalFallbackIsReachableOnlyByExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	i := remoteIntegration(t, root)
	i.localFallback = true

	nodes, err := i.handleDispatchHost(context.Background(), execArgs("v1:planner:plan:p1"), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res := decodeDispatch(t, nodes)
	if !res.OK {
		t.Fatalf("with the fallback explicitly opted into, the call must run locally: %s / %s",
			res.ErrorCode, res.ErrorMsg)
	}
	if entries, _ := os.ReadDir(root); len(entries) == 0 {
		t.Fatal("the opted-in fallback ran nothing locally: the escape valve does not work")
	}
}

// TestLocalFallbackEnvIsOffByDefault pins the default, which is the entire
// safety property. A truthy-parse that treated an unset variable as "on" would
// leave the shipped agent exactly where it started.
func TestLocalFallbackEnvIsOffByDefault(t *testing.T) {
	for _, in := range []string{"", "0", "false", "no", "off", "maybe", " "} {
		if localFallbackEnabled(in) {
			t.Errorf("localFallbackEnabled(%q) = true, want false -- the fallback must be opted INTO", in)
		}
	}
	for _, in := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		if !localFallbackEnabled(in) {
			t.Errorf("localFallbackEnabled(%q) = false, want true", in)
		}
	}
}

// TestSingleNodeModeIsUnaffected is the negative that keeps the change scoped.
//
// An operator who never set MEMQL_WORKBENCH_REMOTE made no assertion about
// where the work runs, so there is nothing to honour and nothing to refuse.
// The MVP path -- the agent node running the workbench in-process -- has to
// keep working exactly as before, or this fix breaks every single-node
// deployment in the name of a cluster-mode guarantee.
func TestSingleNodeModeIsUnaffected(t *testing.T) {
	root := t.TempDir()
	t.Setenv(rootEnvVar, root)
	i := NewIntegration(nil)
	// remote deliberately left false: no assertion was made.

	nodes, err := i.handleDispatchHost(context.Background(), execArgs("v1:planner:plan:p1"), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res := decodeDispatch(t, nodes); !res.OK {
		t.Fatalf("single-node dispatch broke: %s / %s", res.ErrorCode, res.ErrorMsg)
	}
	if entries, _ := os.ReadDir(root); len(entries) == 0 {
		t.Fatal("single-node dispatch provisioned no workspace")
	}
}
