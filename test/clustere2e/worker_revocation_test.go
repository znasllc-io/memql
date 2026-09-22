//go:build clustere2e

package clustere2e

// worker_revocation_test.go -- the live-cluster half of epic memql#5327's
// design D1.
//
// WHAT THE IN-PROCESS GATE CANNOT SAY
// -----------------------------------
// worker_registration_routing_test.go beside this one proves the routing
// DECISION: that `graph.node.*.v1:worker:registration` is forwardable, and
// that a real RegistrationWatcher acts on it when it arrives. It joins two
// buses with a stub link, so what it does NOT prove is that the real mesh --
// NodeService.Stream, the EventBridge, the dedup and TTL beneath it --
// actually delivers the event to a sibling agent replica in a running
// cluster.
//
// That is a deployment fact, and it is the class of fact that has been false
// in production before while every in-process lane stayed green (memql#3506
// is the same shape one component along). So it is asked here, out loud, of a
// real cluster.
//
// It CANNOT RUN WITHOUT ONE and does not run in CI: the package is behind
// //go:build clustere2e and `token(t)` skips without MEMQL_E2E_TOKEN.
//
//	make up SERVERS=2 && make scale N=2 && make cluster-e2e
//
// WHAT IT NEEDS BEYOND THE CLUSTER
// --------------------------------
// A PAIRED MACHINE, which `make up` does not create -- pairing writes a
// credential and the design record for this epic is explicit that a review
// must not mint credentials or pair machines to populate a screen. So this
// lane SKIPS when the signed-in user has no connected machine, and says so
// rather than passing. A skip that reads as a pass is the failure mode the
// whole in-process/live split exists to avoid, which is why the in-process
// half is the gate and this one is the confirmation.

import (
	"context"
	"testing"
	"time"

	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// connectedMachine returns the caller's first machine that a replica is
// currently holding a stream for, or skips.
//
// IT REQUIRES A HELD STREAM, not merely a registration. A revoked row whose
// machine was never connected proves nothing about whether the revoke reached
// the replica holding it -- there was none.
func connectedMachine(ctx context.Context, t *testing.T, qc *memqlclient.QueryClient) (id, holder string) {
	t.Helper()
	res, err := qc.MyWorkersWithStatus(ctx, memqlclient.MyWorkersWithStatusArgs{})
	if err != nil {
		t.Fatalf("read the caller's machines: %v", err)
	}
	for _, row := range res.Rows() {
		node, _ := row["connectedNodeId"].(string)
		revoked, _ := row["revokedAt"].(string)
		rowId, _ := row["id"].(string)
		if node != "" && revoked == "" && rowId != "" {
			return rowId, node
		}
	}
	t.Skip("no connected machine on this cluster -- pair one with `memql worker pair` and run again. " +
		"This lane deliberately does not pair one itself: a review must not mint credentials to " +
		"populate a screen, and the in-process half (worker_registration_routing_test.go) is the gate.")
	return "", ""
}

func TestARevokeReachesTheReplicaHoldingTheStream(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn := openConnections(ctx, t, tok, 1)[0]
	defer conn.Close()
	qc := memqlclient.NewQueryClient(conn.Dispatcher())

	machineId, holder := connectedMachine(ctx, t, qc)
	t.Logf("machine %s is held by %s", machineId, holder)

	// ONE ACT (design D2): the registration and the credential together. This
	// is also what the OS button calls, so a green lane here is evidence about
	// the path a person actually takes.
	if _, err := qc.FleetRevokeMachine(ctx, memqlclient.FleetRevokeMachineArgs{
		RegistrationId: machineId,
		Reason:         "clustere2e: design D1",
	}); err != nil {
		t.Fatalf("fleetRevokeMachine: %v", err)
	}

	// THE ASSERTION IS THAT THE STAMP GOES, and it is the right one to make
	// from outside the cluster. `connectedNodeId` is cleared by the session's
	// own teardown on the replica that held the stream -- so a cleared stamp
	// is that replica saying, in the only way a client can observe, that it
	// heard about the revoke and let the connection go.
	//
	// A machine that merely stopped heartbeating would NOT clear it: the
	// stale-hold sweep (design D7) runs every two minutes, which is well
	// outside this deadline. So a pass inside the window is the watcher, not
	// the sweep.
	deadline := time.Now().Add(45 * time.Second)
	for {
		res, err := qc.MyWorkersWithStatus(ctx, memqlclient.MyWorkersWithStatusArgs{})
		if err != nil {
			t.Fatalf("re-read the caller's machines: %v", err)
		}
		for _, row := range res.Rows() {
			if rowId, _ := row["id"].(string); rowId != machineId {
				continue
			}
			if node, _ := row["connectedNodeId"].(string); node == "" {
				return // the stream ended and its holder said so
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still names %s as holding its stream 45s after the revoke -- the "+
				"registration write did not reach that replica through the mesh, so the machine "+
				"is out of routing with its connection still open", machineId, holder)
		}
		time.Sleep(2 * time.Second)
	}
}
