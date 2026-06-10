package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/server"
)

// newLifecycleTestApp builds a minimal App wired the way cluster.go wires the
// node lifecycle: a PeerManager-owned lifecycle plus the observer that bridges
// Draining/Stopped to the readiness surface (server.SetDraining). It does NOT
// stand up the full dependency graph -- this exercises only the lifecycle <->
// readiness contract.
func newLifecycleTestApp(t *testing.T) *App {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := node.NewPeerManager(&node.Identity{ID: "n1", Type: node.NodeTypeBFF}, logger)

	a := &App{
		Logger:        logger,
		nodeIdentity:  &node.Identity{ID: "n1", Type: node.NodeTypeBFF},
		nodeLifecycle: pm.Lifecycle(),
	}
	// Mirror cluster.go's observer wiring.
	a.nodeLifecycle.SetObserver(func(_, newState node.LifecycleState) {
		if newState == node.LifecycleDraining || newState == node.LifecycleStopped {
			server.SetDraining(true)
		}
	})
	return a
}

// TestNodeLifecycle_ReadinessFlipsOnDraining is the core memql#1268 acceptance:
// readiness (server.IsDraining, consulted by /healthz + /readyz) flips to
// not-ready the moment the node lifecycle goes Draining -- while the node is
// still alive (liveness is /livez, which never consults draining).
func TestNodeLifecycle_ReadinessFlipsOnDraining(t *testing.T) {
	server.SetDraining(false)
	t.Cleanup(func() { server.SetDraining(false) })

	a := newLifecycleTestApp(t)

	// Boot -> Ready: readiness must report ready (not draining).
	a.MarkNodeReady()
	require.True(t, a.nodeLifecycle.IsReady(), "node should be Ready after MarkNodeReady")
	assert.False(t, server.IsDraining(), "a Ready node must NOT report draining on readiness")

	// Drain: readiness flips to not-ready.
	a.BeginNodeDrain()
	assert.True(t, a.nodeLifecycle.IsDraining(), "node should be Draining after BeginNodeDrain")
	assert.True(t, server.IsDraining(), "a Draining node MUST report draining on readiness")
}

// TestNodeLifecycle_DrainFromStartingStillFlipsReadiness covers a SIGTERM
// arriving mid-boot (before Ready): the drain must still de-route the pod.
func TestNodeLifecycle_DrainFromStartingStillFlipsReadiness(t *testing.T) {
	server.SetDraining(false)
	t.Cleanup(func() { server.SetDraining(false) })

	a := newLifecycleTestApp(t)
	// No MarkNodeReady -- still Starting.
	require.Equal(t, node.LifecycleStarting, a.nodeLifecycle.State())

	a.BeginNodeDrain()
	assert.True(t, a.nodeLifecycle.IsDraining())
	assert.True(t, server.IsDraining(), "draining from Starting must still flip readiness")
}

// TestNodeLifecycle_NoMeshIsNoop asserts the lifecycle hooks are safe no-ops on
// a non-mesh binary (no PeerManager -> nil lifecycle): they must not panic and
// must not touch the readiness flag.
func TestNodeLifecycle_NoMeshIsNoop(t *testing.T) {
	server.SetDraining(false)
	t.Cleanup(func() { server.SetDraining(false) })

	a := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.MarkNodeReady()   // must not panic
	a.BeginNodeDrain()  // must not panic
	assert.False(t, server.IsDraining(), "no-mesh drain must not flip readiness")
}
