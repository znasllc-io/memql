//go:build clustere2e

package clustere2e

// deploy_control_forward_test.go -- memql#3380, the live-cluster half of the
// deploy-control cross-node gate.
//
// The in-process gate (component/grpc/deploy_control_forward_test.go) proves
// the two halves agree about the wire and that the role matrix survives the
// hop. It cannot prove the hop is REACHABLE in a real cluster -- that the bff
// dials the identity node at all, that the identity node's NodeServer accepts
// the bff's class="node" credential on this message, and that the reply finds
// its way back through the WorkerDialer's inbound sink. Those are deployment
// facts, and this is where they are checked.
//
// It CANNOT RUN WITHOUT A LIVE CLUSTER and does not run in CI: the package is
// behind //go:build clustere2e and `token(t)` skips without MEMQL_E2E_TOKEN.
//
//	make up && make cluster-e2e
//
// The endpoint is the bff front door, which is the whole point: the portal is
// served by the bff and a SPA dials the origin that served it, so this is the
// exact path an operator's browser takes.

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// TestDeployControlReachesTheIdentityNodeFromABff is the acceptance check for
// the reported symptom: the portal's live-state read answered UNIMPLEMENTED.
//
// The assertion is deliberately about the CODE rather than the payload. What
// the deploy status contains depends on the cluster's overlays and on whether
// kubectl can see Argo, and a test that asserted on that would be asserting on
// the environment. What must be true regardless is that the bff no longer
// claims the surface does not exist:
//
//   - UNIMPLEMENTED means the forward is not wired, or the bff cannot see the
//     identity node -- the bug.
//   - UNAVAILABLE means the route exists but no identity peer was reachable at
//     that moment. Reported loudly: on a healthy cluster it is a real failure
//     of the dial path, not a pass.
//   - OK, or any gate/argument code, means the call reached the service on the
//     identity node and was answered there. That is the property under test.
func TestDeployControlReachesTheIdentityNodeFromABff(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()

	requestId := "deploy-status-" + id.NewShortId()
	reply, err := conns[0].Dispatcher().SendAndWait(ctx, &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_DeployControl{
			DeployControl: &memqlv1.DeployControlMsg{
				RequestId: requestId,
				Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
					GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "staging"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("deploy_control over the bff stream: %v", err)
	}

	res := reply.GetDeployControlResult()
	if res == nil {
		t.Fatalf("reply carried no DeployControlResult: %T", reply.GetPayload())
	}
	code := codes.Code(res.GetErrorCode())

	if !res.GetOk() && code == codes.Unimplemented {
		t.Fatalf("the bff answered UNIMPLEMENTED: the deploy surface is not reaching the identity "+
			"node (memql#3380). message=%q", res.GetErrorMessage())
	}
	if !res.GetOk() && code == codes.Unavailable {
		t.Fatalf("the bff could not reach an identity peer: the route is wired but the dial is not "+
			"up. Check that identity registered a v1:cluster:node row with an address, and that the "+
			"bff's MEMQL_WORKER_PEERS seed names it. message=%q", res.GetErrorMessage())
	}

	// Anything else -- ok, PermissionDenied from the owner/admin read gate, an
	// argument error -- was decided by the service on the identity node, which
	// is what "the hop works" means.
	t.Logf("deploy status answered by the identity node: ok=%v code=%v", res.GetOk(), code)
	if res.GetOk() {
		if got := res.GetDeploymentStatus().GetEnv(); got != "staging" {
			t.Errorf("status env = %q, want staging", got)
		}
	}
}

// TestForwardedRollbackRefusesANonOwnerInTheCluster is the live-cluster mirror
// of the in-process role-matrix gate.
//
// It runs ONLY when the operator supplies a non-owner token via
// MEMQL_E2E_ADMIN_TOKEN, because the property is "a caller who is not the owner
// is refused" and the default MEMQL_E2E_TOKEN is the cluster owner. Skipping is
// the honest outcome without one: asserting a refusal against an owner token
// would pass for the wrong reason.
func TestForwardedRollbackRefusesANonOwnerInTheCluster(t *testing.T) {
	tok := nonOwnerToken(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()

	requestId := "deploy-rollback-" + id.NewShortId()
	reply, err := conns[0].Dispatcher().SendAndWait(ctx, &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_DeployControl{
			DeployControl: &memqlv1.DeployControlMsg{
				RequestId: requestId,
				Request: &memqlv1.DeployControlMsg_RollbackDeployment{
					RollbackDeployment: &memqlv1.RollbackDeploymentRequest{
						ToDeploymentId: "e2e-nonexistent-" + id.NewShortId(),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("deploy_control rollback over the bff stream: %v", err)
	}
	res := reply.GetDeployControlResult()
	if res == nil {
		t.Fatalf("reply carried no DeployControlResult: %T", reply.GetPayload())
	}
	if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
		t.Fatalf("a non-owner rollback came back %v, want PermissionDenied -- rollback is owner-only "+
			"and the gate must run on the identity node with the ORIGINATING caller's role. message=%q",
			got, res.GetErrorMessage())
	}
}

// nonOwnerToken returns a JWT for a caller below owner, or skips.
func nonOwnerToken(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("MEMQL_E2E_ADMIN_TOKEN"); v != "" {
		return v
	}
	t.Skip("MEMQL_E2E_ADMIN_TOKEN not set -- the owner-only rollback gate needs a NON-owner caller " +
		"to be meaningful; export a JWT for an admin user to run this")
	return ""
}
