package node

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParentConnector_NilWhenNoParent: the constructor returns nil when there
// is no parent address, so callers can blindly append without branching.
func TestParentConnector_NilWhenNoParent(t *testing.T) {
	logger := testLogger()
	pm := NewPeerManager(&Identity{ID: "n1", Type: NodeTypeBFF}, logger)

	assert.Nil(t, NewParentConnector(&Identity{ID: "n1"}, pm, logger),
		"no parent address -> nil ParentConnector")
	assert.Nil(t, NewParentConnector(nil, pm, logger),
		"nil identity -> nil ParentConnector")
}

// TestParentConnector_StaysRunningAcrossParentDisconnect is the memql#1246
// regression guard. The parent is UNREACHABLE (nothing listening), so every
// dial fails -- the staging analogue of identity rolling underneath a child.
// The resilient run loop (memql#1269/#1246) must keep the component
// IsRunning()==true across reconnect attempts: it must NOT terminate the run
// hook (which would flip IsRunning() false permanently and wedge /healthz at
// 503 / 0-1, the exact outage). Readiness reflects serving-capability, not
// "parent currently connected".
func TestParentConnector_StaysRunningAcrossParentDisconnect(t *testing.T) {
	logger := testLogger()
	identity := &Identity{
		ID:            "child-1",
		Type:          NodeTypeCognition,
		ParentAddress: "127.0.0.1:1", // nothing listens here -> dials always fail
	}
	pm := NewPeerManager(identity, logger)

	pc := NewParentConnector(identity, pm, logger)
	require.NotNil(t, pc)

	pc.Start(context.Background())
	t.Cleanup(func() { pc.Stop(context.Background()) })

	// It comes up (markStarted fired) ...
	require.Eventually(t, pc.IsRunning, 2*time.Second, 10*time.Millisecond,
		"ParentConnector should report running shortly after Start")

	// ... and STAYS up across multiple internal + outer reconnect cycles, even
	// though the parent is never reachable. A flaky/terminating run loop would
	// flip this false (the #1246 bug).
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		assert.True(t, pc.IsRunning(),
			"ParentConnector must stay running across parent reconnects (memql#1246)")
	}
}

// TestParentConnector_StopIsClean: Stop cancels the context, the supervising
// run loop exits cleanly (ctx.Err() != nil path), and IsRunning() flips false.
func TestParentConnector_StopIsClean(t *testing.T) {
	logger := testLogger()
	identity := &Identity{
		ID:            "child-2",
		Type:          NodeTypeCognition,
		ParentAddress: "127.0.0.1:1",
	}
	pm := NewPeerManager(identity, logger)

	pc := NewParentConnector(identity, pm, logger)
	require.NotNil(t, pc)

	pc.Start(context.Background())
	require.Eventually(t, pc.IsRunning, 2*time.Second, 10*time.Millisecond)

	pc.Stop(context.Background())
	assert.False(t, pc.IsRunning(),
		"ParentConnector must report not-running after a clean Stop")
}
