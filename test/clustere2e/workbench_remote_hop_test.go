//go:build clustere2e

package clustere2e

// workbench_remote_hop_test.go -- memql#3506, the agent-to-workbench hop that
// memql#3450 left open.
//
// The in-process tests (integrations/workbench/remote_required_test.go) prove
// the agent REFUSES when no workbench peer is reachable. They cannot prove the
// opposite half -- that when a peer IS reachable the work actually lands on it.
// That is a deployment fact, and it is exactly the fact that was false in
// production for the whole life of #3450: agent.yaml set both
// MEMQL_WORKBENCH_REMOTE=1 and the peer seed, the seed was dropped at parse
// time, and every workbench call ran on the agent pod with correct-looking
// results.
//
// Nothing in CI could see that, because nothing in CI asked WHICH MACHINE ran
// the command. This test asks.
//
// It CANNOT RUN WITHOUT A LIVE CLUSTER and does not run in CI: the package is
// behind //go:build clustere2e and `token(t)` skips without MEMQL_E2E_TOKEN.
//
//	make up && make cluster-e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// buildWorkbenchExec renders a `workbenchDispatchHost` call running one
// allowlisted command in the per-Plan workspace.
//
// The builtin is the right level to probe at. `workbenchHost` -- the TOOL the
// agent calls -- is defined in a product's DSL bundle, and the engine ships
// product-agnostic (memql#2472), so a test in this repo that went through the
// tool surface would be testing whether a bundle happened to be mounted. The
// builtin is engine-owned and sits directly on top of the integration
// capability, which is the seam the hop actually crosses.
func buildWorkbenchExec(planID, cmd string) string {
	// @args(profile="object") -- the builtin takes one JSON object, not a
	// bare string or a named-arg list. Hop tests that passed
	// workbenchDispatchHost("env") (or the named-arg form) fail argument
	// validation (memql#4212).
	return fmt.Sprintf(`workbenchDispatchHost({action: %q, planId: %q, args: {cmd: %q}})`, "exec", planID, cmd)
}

// workbenchDispatch is the dispatchResult shape the integration returns,
// restated here because the type is unexported in its own package.
type workbenchDispatch struct {
	OK        bool           `json:"ok"`
	Action    string         `json:"action"`
	ErrorCode string         `json:"errorCode"`
	ErrorMsg  string         `json:"errorMessage"`
	Payload   map[string]any `json:"payload"`
}

// runWorkbenchExec dispatches one exec and decodes the single result node.
func runWorkbenchExec(ctx context.Context, t *testing.T, qc *memqlclient.QueryClient, planID, cmd string) workbenchDispatch {
	t.Helper()
	res, err := qc.ExecuteNamed(ctx, "workbenchDispatchHost", buildWorkbenchExec(planID, cmd))
	if err != nil {
		t.Fatalf("workbenchDispatchHost(%q): %v", cmd, err)
	}
	rows := res.Rows()
	if len(rows) != 1 {
		t.Fatalf("want exactly one dispatch row, got %d", len(rows))
	}
	raw, mErr := json.Marshal(rows[0])
	if mErr != nil {
		t.Fatalf("re-marshalling the dispatch row: %v", mErr)
	}
	// The row carries the dispatchResult either inline or under `payload`
	// depending on how the engine renders an integration node; try the
	// envelope first and fall back to the row itself.
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	body := raw
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Payload) > 0 {
		body = envelope.Payload
	}
	var out workbenchDispatch
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the dispatch result from %s: %v", string(raw), err)
	}
	return out
}

// TestWorkbenchExecRunsOnTheWorkbenchNodeNotTheAgent is the acceptance test for
// the hop.
//
// The probe is `env`, and the discriminator is MEMQL_NODE_ID -- which every
// deployment stamps from the pod's own `metadata.name` (deploy/k8s/base/*.yaml,
// `fieldRef: metadata.name`). A command that ran on the workbench reports a
// workbench pod; one that ran on the agent reports an agent pod. There is no
// way to satisfy this assertion by accident, which is the property #3450 needed
// and did not have.
//
// Asserting on the node id rather than on the command's OUTPUT is the point.
// `echo hello` returns "hello" from either machine, and that is precisely how a
// silently-local workbench looked correct for months.
func TestWorkbenchExecRunsOnTheWorkbenchNodeNotTheAgent(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	planID := "v1:planner:plan:" + id.NewShortId()

	res := runWorkbenchExec(ctx, t, qc, planID, "env")
	if !res.OK {
		// A refusal here is itself the memql#3506 signal, and it is worth
		// reporting in full: on a cluster whose agent has MEMQL_WORKBENCH_REMOTE
		// set, `no_workbench_peer` means the workbench is genuinely unreachable
		// -- which is a real cluster defect the old fallback would have hidden.
		t.Fatalf("workbench exec failed: %s / %s", res.ErrorCode, res.ErrorMsg)
	}

	stdout, _ := res.Payload["stdout"].(string)
	nodeID := ""
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MEMQL_NODE_ID="); ok {
			nodeID = v
			break
		}
	}
	if nodeID == "" {
		t.Fatalf("the exec's environment carries no MEMQL_NODE_ID, so this test cannot tell which node ran "+
			"the command -- which is the only thing it exists to check.\nstdout:\n%s", stdout)
	}

	if strings.HasPrefix(nodeID, "agent") {
		t.Fatalf("the workbench command ran on the AGENT node (%s). Either MEMQL_WORKBENCH_REMOTE is not set "+
			"on the agent, or MEMQL_WORKBENCH_LOCAL_FALLBACK is on and the workbench peer is unreachable. "+
			"This is the memql#3450 shape: the operator asked for isolation and the work ran on the agent's "+
			"own disk", nodeID)
	}
	if !strings.HasPrefix(nodeID, "workbench") {
		t.Fatalf("the workbench command ran on %q, which is neither an agent nor a workbench pod; "+
			"the hop is landing somewhere unexpected", nodeID)
	}
}

// TestWorkbenchWorkspacePersistsAcrossCallsOnTheRemoteNode is the second half
// of "the hop works": the per-Plan workspace is a real, persistent directory on
// the workbench node, not a fresh temp dir per call.
//
// It also happens to be a second, independent witness that both calls reached
// the SAME node. A cluster that round-robined between two workbench replicas
// would fail here rather than silently splitting a Plan's files across two
// filesystems -- which would be the next member of this bug family, and is
// worth failing loudly on rather than discovering later.
func TestWorkbenchWorkspacePersistsAcrossCallsOnTheRemoteNode(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	planID := "v1:planner:plan:" + id.NewShortId()
	marker := "workbench-hop-probe-" + id.NewShortId()

	if res := runWorkbenchExec(ctx, t, qc, planID, "echo "+marker+" > marker.txt"); !res.OK {
		t.Fatalf("writing the marker: %s / %s", res.ErrorCode, res.ErrorMsg)
	}

	res := runWorkbenchExec(ctx, t, qc, planID, "cat marker.txt")
	if !res.OK {
		t.Fatalf("reading the marker back: %s / %s -- the second call did not see the first call's "+
			"workspace, so the per-Plan directory is not persisting on the workbench node",
			res.ErrorCode, res.ErrorMsg)
	}
	stdout, _ := res.Payload["stdout"].(string)
	if !strings.Contains(stdout, marker) {
		t.Fatalf("marker.txt does not contain %q (stdout: %q)", marker, stdout)
	}
}
