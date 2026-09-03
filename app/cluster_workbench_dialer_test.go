package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/node"
)

// cluster_workbench_dialer_test.go pins ONE outbound stream per peer.
//
// ===========================================================================
// THE BUG THIS EXISTS TO STOP COMING BACK
// ===========================================================================
// Two files build a WorkerDialer -- cluster.go for the bff's AI and
// deploy-control routes, cluster_workbench.go for the build route -- and
// neither owns the other. When epic memql#4900 taught the bff to forward
// workbench calls, the obvious wiring (copy the agent's branch) gave the bff a
// SECOND dialer over the same seed list, so one process would have opened two
// outbound NodeService streams to the same workbench, run two reconcile loops
// over one connection set, and registered two event-bus subscriptions under
// one name.
//
// Nothing would have failed loudly. That is the reason for a test rather than
// a comment: the symptom is churn, and churn is what a flaky bring-up looks
// like -- which is exactly the evidence that would be dismissed.

func dialerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// bffApp is an App wired as far as the workbench forwarding needs: a peer
// manager, and the dialer cluster.go would already have installed.
func bffApp(t *testing.T, withExistingDialer bool) (*App, *node.Identity, *node.PeerManager) {
	t.Helper()
	identity := node.NewIdentity("test")
	identity.Type = node.NodeTypeBFF
	identity.ID = "bff-test-1"
	peers := node.NewPeerManager(identity, dialerLogger())
	// A seed is what makes NewWorkerDialer return something: with no engine,
	// no event bus and no static targets it has nothing to reconcile from and
	// answers nil. The same seed list is what the bff really carries.
	seeds := []node.WorkerTarget{{NodeType: node.NodeTypeWorkbench, Address: "workbench:50060"}}
	t.Setenv("MEMQL_WORKER_PEERS", "identity=identity:50061,workbench=workbench:50060")
	a := &App{Logger: dialerLogger()}
	if withExistingDialer {
		d := node.NewWorkerDialer(identity, peers, nil, nil, seeds, dialerLogger())
		if d == nil {
			t.Fatal("the fixture needs a dialer to stand in for the one cluster.go installs")
		}
		a.Dependencies = append(a.Dependencies, d)
	}
	return a, identity, peers
}

func countDialers(a *App) int {
	n := 0
	for _, dep := range a.Dependencies {
		if _, ok := dep.(*node.WorkerDialer); ok {
			n++
		}
	}
	return n
}

func TestTheBffKeepsOneWorkerDialer(t *testing.T) {
	// Remote mode on, which is what deploy/k8s/components/engine-bff sets: the
	// branch that used to add a dialer.
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "1")

	a, identity, peers := bffApp(t, true)
	before := countDialers(a)
	a.wireWorkbenchForwarding(identity, peers, nil, nil)

	if got := countDialers(a); got != before {
		t.Fatalf("the bff already has a dialer, so workbench forwarding must reuse it; dialers %d -> %d", before, got)
	}
}

func TestANodeWithNoDialerStillGetsOne(t *testing.T) {
	// The reachable positive. Without it the assertion above would pass
	// against wiring that had stopped installing a dialer at all, which would
	// leave the router with nothing to send on -- and every build refused for
	// a reason naming the wrong thing.
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "1")

	a, identity, peers := bffApp(t, false)
	a.wireWorkbenchForwarding(identity, peers, nil, nil)

	if got := countDialers(a); got != 1 {
		t.Fatalf("a node with no dialer must be given exactly one, got %d", got)
	}
}

func TestRemoteModeOffInstallsNothing(t *testing.T) {
	// The flag is the whole switch: with it unset the node keeps its
	// single-node behaviour and this wiring is a no-op.
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "")

	a, identity, peers := bffApp(t, false)
	a.wireWorkbenchForwarding(identity, peers, nil, nil)

	if got := countDialers(a); got != 0 {
		t.Fatalf("with remote mode off nothing may be installed, got %d dialers", got)
	}
}
