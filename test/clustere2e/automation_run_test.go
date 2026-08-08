//go:build clustere2e

package clustere2e

// automation_run_test.go -- memql#3310, the live-cluster half of the
// automation invoke path's cross-node gate.
//
// The deterministic, cluster-free half lives next door in
// automation_run_routing_test.go, which drives two in-process relays through
// the real routing decision and fails when the rule is removed. This file is
// the same property against the real mesh: real EventBridge, real gRPC peers,
// real structpb encoding, real replicas.
//
// It CANNOT RUN WITHOUT A LIVE 2-REPLICA CLUSTER and does not run in CI: the
// whole file is behind //go:build clustere2e and `token(t)` skips without
// MEMQL_E2E_TOKEN. Run it with
//
//	make up SERVERS=2 && make scale N=2 && make cluster-e2e
//
// SIDE EFFECTS: NONE, DELIBERATELY.
// A run executes an automation's whole action chain -- writes, LLM calls,
// downstream automations -- so a gate that fires a real automation on every
// invocation would be a gate nobody dares run. The probe therefore names an
// automation that does not exist. That is not a weaker test: the hop being
// exercised is the RELAY, and a relayed refusal proves every link of it. The
// request has to reach the cognition node, that node has to consult its own
// registry, and its answer has to travel back and terminate the caller's
// stream. The only thing that does not happen is the automation body, which is
// the only thing this test does not want to happen.
//
// The failure signatures are distinct and that is the whole design:
//
//	NOT_FOUND    (5)  from executed_on_node_type=cognition -> the hop worked
//	UNAVAILABLE (14)  with no executing node             -> the hop did NOT
//
// A missing routing rule produces the second, every time, because cross-node
// event delivery is default-deny in component/node.

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// Canonical gRPC codes, named here so the assertions read as the contract
// rather than as magic numbers.
const (
	codeNotFound    = 5
	codeUnavailable = 14
	codePermDenied  = 7
)

// A run requested on the bff must execute on the node type it targets, and its
// trace must come back.
func TestAutomationRunCrossesTheMesh(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	disp := conns[0].Dispatcher()

	requestId := "e2e-automation-run-" + id.NewShortId()
	frames, release := disp.RegisterStream(requestId)
	defer release()

	if _, err := disp.Send(&memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_RunAutomation{
			RunAutomation: &memqlv1.RunAutomationMsg{
				RequestId: requestId,
				// Deliberately not a real automation -- see the file header.
				Automation: "e2eCrossNodeProbe" + id.NewShortId(),
				// The hop under test. cognition is a node type the local and
				// staging overlays both run, and it is never the node type
				// serving this stream (that is bff), so the run cannot
				// accidentally satisfy itself locally.
				TargetNodeType: "cognition",
				TimeoutMs:      60000,
			},
		},
	}); err != nil {
		t.Fatalf("send RunAutomation: %v", err)
	}

	var accepted *memqlv1.AutomationRunAccepted
	var done *memqlv1.AutomationRunComplete
	stepCount := 0

	deadline := time.After(90 * time.Second)
	for done == nil {
		select {
		case <-deadline:
			t.Fatal("no AutomationRunComplete frame within 90s -- the run never terminated. " +
				"Every run must emit exactly one complete frame; a caller parks on it.")
		case <-ctx.Done():
			t.Fatalf("context: %v", ctx.Err())
		case msg, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed before the run completed")
			}
			evt := msg.GetAutomationRunEvent()
			if evt == nil {
				// A refusal from an interceptor arrives as a QueryError; name
				// it rather than timing out on it.
				if qe := msg.GetQueryError(); qe != nil {
					t.Fatalf("RunAutomation was rejected before reaching the handler: %s", qe.GetError().GetMessage())
				}
				continue
			}
			switch {
			case evt.GetAccepted() != nil:
				accepted = evt.GetAccepted()
			case evt.GetStep() != nil:
				stepCount++
			case evt.GetComplete() != nil:
				done = evt.GetComplete()
			}
		}
	}

	if accepted == nil {
		t.Fatal("the run produced no accepted frame; every run opens with one")
	}
	if done.GetErrorCode() == codePermDenied {
		t.Skipf("MEMQL_E2E_TOKEN is not an owner/admin, which the run surface requires: %s",
			done.GetErrorMessage())
	}

	// THE GATE.
	if done.GetErrorCode() == codeUnavailable {
		t.Fatalf("the cross-node run was never picked up: %s\n\n"+
			"This is what a MISSING ROUTING RULE looks like in the real mesh. "+
			"Cross-node event delivery is default-deny in component/node; without the "+
			"automationrun.# forward rule (component/node/routing_automation_run.go) the "+
			"relay request never leaves the bff replica that took it and no cognition node "+
			"ever sees it. Also check that a cognition Deployment is actually running "+
			"(kubectl -n memql get deploy).", done.GetErrorMessage())
	}
	if done.GetErrorCode() != codeNotFound {
		t.Fatalf("want NOT_FOUND from the cognition node's own registry, got [%d] %s (status %q)",
			done.GetErrorCode(), done.GetErrorMessage(), done.GetStatus())
	}
	if done.GetExecutedOnNodeType() != "cognition" {
		t.Fatalf("the run must have been answered by a cognition node, got %q/%q",
			done.GetExecutedOnNodeType(), done.GetExecutedOnNodeId())
	}
	if done.GetExecutedOnNodeId() == "" {
		t.Fatal("the executing node must identify itself, so the UI can name the cluster and node")
	}
	if accepted.GetRequestedOnNodeId() == done.GetExecutedOnNodeId() {
		t.Fatalf("requesting and executing node are the same (%q) -- this test did not exercise a hop",
			done.GetExecutedOnNodeId())
	}
	if stepCount != 0 {
		t.Fatalf("a refused run must emit no step frames, got %d", stepCount)
	}

	// The banner the UI is required to render has to survive the mesh hop.
	if !accepted.GetRanDeployedDefinition() || strings.TrimSpace(accepted.GetDefinitionNote()) == "" {
		t.Errorf("the deployed-definition banner must arrive as DATA, got %+v", accepted)
	}
}

// A LOCAL run (no target node type) must answer from the node that took the
// request. This is the control for the test above: if it also reported a
// cognition node, the target routing would be doing nothing and the hop
// assertion would be meaningless.
func TestAutomationRunWithoutTargetStaysLocal(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	disp := conns[0].Dispatcher()

	requestId := "e2e-automation-run-local-" + id.NewShortId()
	frames, release := disp.RegisterStream(requestId)
	defer release()

	if _, err := disp.Send(&memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_RunAutomation{
			RunAutomation: &memqlv1.RunAutomationMsg{
				RequestId:  requestId,
				Automation: "e2eLocalProbe" + id.NewShortId(),
				TimeoutMs:  30000,
			},
		},
	}); err != nil {
		t.Fatalf("send RunAutomation: %v", err)
	}

	var accepted *memqlv1.AutomationRunAccepted
	var done *memqlv1.AutomationRunComplete
	deadline := time.After(45 * time.Second)
	for done == nil {
		select {
		case <-deadline:
			t.Fatal("no AutomationRunComplete frame within 45s")
		case msg, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed before the run completed")
			}
			evt := msg.GetAutomationRunEvent()
			if evt == nil {
				continue
			}
			if a := evt.GetAccepted(); a != nil {
				accepted = a
			}
			if c := evt.GetComplete(); c != nil {
				done = c
			}
		}
	}

	if done.GetErrorCode() == codePermDenied {
		t.Skipf("MEMQL_E2E_TOKEN is not an owner/admin: %s", done.GetErrorMessage())
	}
	if done.GetErrorCode() != codeNotFound {
		t.Fatalf("want NOT_FOUND, got [%d] %s", done.GetErrorCode(), done.GetErrorMessage())
	}
	if accepted == nil || accepted.GetRequestedOnNodeId() == "" {
		t.Fatalf("the accepted frame must name the node that took the request, got %+v", accepted)
	}
	if done.GetExecutedOnNodeId() != accepted.GetRequestedOnNodeId() {
		t.Fatalf("an untargeted run must answer from the receiving node; requested on %q, executed on %q",
			accepted.GetRequestedOnNodeId(), done.GetExecutedOnNodeId())
	}
}
