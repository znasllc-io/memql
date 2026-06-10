package memql

// Tests for handleNodeMaintenance -- the owner/admin-gated operator
// maintenance trigger (memql#1270). They prove (1) the auth gate rejects a
// non-owner/admin caller, (2) an owner/admin caller funnels into the wired
// drain handler (the same #1269 mechanism), (3) a node with no maintenance
// entrypoint reports unavailable, and (4) an unknown action is rejected.

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// fakeMaintenanceHandler records BeginDrain calls so the tests can assert
// the trigger reached the lifecycle driver.
type fakeMaintenanceHandler struct {
	mu             sync.Mutex
	nodeID         string
	state          string
	beginCalls     int
	beginReasons   []string
	alreadyDrained bool // when true, BeginDrain returns false (already under way)
}

func (f *fakeMaintenanceHandler) NodeID() string       { return f.nodeID }
func (f *fakeMaintenanceHandler) CurrentState() string { return f.state }
func (f *fakeMaintenanceHandler) BeginDrain(reason string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginCalls++
	f.beginReasons = append(f.beginReasons, reason)
	return !f.alreadyDrained
}

// newMaintenanceSession builds a streamSession with a pre-seeded
// AccessContext (so the owner/admin gate resolves without a DB) and the
// supplied maintenance handler.
func newMaintenanceSession(t *testing.T, role auth.Role, h NodeMaintenanceHandler) (*streamSession, *captureStream) {
	t.Helper()
	cs := newCaptureStream(t)
	svc := &service{
		logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		nodeMaintenanceHandler: h,
	}
	s := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		access:  &auth.AccessContext{UserId: "u1", Role: role},
	}
	s.accessLoaded = true // short-circuit ensureAccess to the seeded access
	return s, cs
}

func maintenanceResult(t *testing.T, cs *captureStream) *memqlv1.NodeMaintenanceResult {
	t.Helper()
	msg := cs.lastSent()
	require.NotNil(t, msg, "handler must send a reply")
	res := msg.GetNodeMaintenanceResult()
	require.NotNil(t, res, "reply must be a NodeMaintenanceResult")
	return res
}

func dispatchMaintenance(s *streamSession, action, reason string) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_NodeMaintenance{
			NodeMaintenance: &memqlv1.NodeMaintenanceMsg{
				RequestId: "r1",
				Action:    action,
				Reason:    reason,
			},
		},
	}
	return s.handleNodeMaintenance(env, env.GetNodeMaintenance())
}

// TestNodeMaintenance_OwnerTriggersDrain: an owner funnels into BeginDrain
// and gets ok=true with the node id + state echoed.
func TestNodeMaintenance_OwnerTriggersDrain(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "draining"}
	s, cs := newMaintenanceSession(t, auth.RoleOwner, h)

	require.NoError(t, dispatchMaintenance(s, "drain", "manual roll"))

	res := maintenanceResult(t, cs)
	assert.True(t, res.GetOk(), "owner drain must be accepted")
	assert.False(t, res.GetAlreadyDraining())
	assert.Equal(t, "bff-2", res.GetNodeId())
	assert.Equal(t, "draining", res.GetState())
	assert.Equal(t, 1, h.beginCalls, "the trigger must funnel into BeginDrain exactly once")
	assert.Equal(t, []string{"manual roll"}, h.beginReasons)
}

// TestNodeMaintenance_AdminTriggersDrain: admin is also allowed (the gate
// is owner OR admin).
func TestNodeMaintenance_AdminTriggersDrain(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "cognition-1", state: "draining"}
	s, cs := newMaintenanceSession(t, auth.RoleAdmin, h)

	require.NoError(t, dispatchMaintenance(s, "drain", ""))

	res := maintenanceResult(t, cs)
	assert.True(t, res.GetOk(), "admin drain must be accepted")
	assert.Equal(t, 1, h.beginCalls)
}

// TestNodeMaintenance_NonAdminRejected: a writer/reader is rejected with
// permission_denied and the drain is NEVER triggered (no open endpoint).
func TestNodeMaintenance_NonAdminRejected(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		t.Run(string(role), func(t *testing.T) {
			h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "ready"}
			s, cs := newMaintenanceSession(t, role, h)

			require.NoError(t, dispatchMaintenance(s, "drain", "sneaky"))

			res := maintenanceResult(t, cs)
			assert.False(t, res.GetOk(), "non-admin must be rejected")
			assert.Equal(t, "permission_denied", res.GetErrorCode())
			assert.Equal(t, 0, h.beginCalls, "a rejected caller must NEVER reach the drain")
		})
	}
}

// TestNodeMaintenance_NoAccessRejected: a missing AccessContext fails
// closed.
func TestNodeMaintenance_NoAccessRejected(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "ready"}
	cs := newCaptureStream(t)
	svc := &service{
		logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		nodeMaintenanceHandler: h,
	}
	s := &streamSession{service: svc, stream: cs, logger: svc.logger}
	s.accessLoaded = true // access stays nil -> fail closed

	require.NoError(t, dispatchMaintenance(s, "drain", ""))
	res := maintenanceResult(t, cs)
	assert.False(t, res.GetOk())
	assert.Equal(t, "permission_denied", res.GetErrorCode())
	assert.Equal(t, 0, h.beginCalls)
}

// TestNodeMaintenance_AlreadyDraining: a second operator call (or a SIGTERM
// race) is reported as already_draining, ok=true.
func TestNodeMaintenance_AlreadyDraining(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "draining", alreadyDrained: true}
	s, cs := newMaintenanceSession(t, auth.RoleOwner, h)

	require.NoError(t, dispatchMaintenance(s, "drain", ""))

	res := maintenanceResult(t, cs)
	assert.True(t, res.GetOk(), "already-draining is a no-op success")
	assert.True(t, res.GetAlreadyDraining())
}

// TestNodeMaintenance_UnavailableWhenUnwired: a node with no maintenance
// entrypoint (non-mesh binary) reports unavailable even for an owner.
func TestNodeMaintenance_UnavailableWhenUnwired(t *testing.T) {
	s, cs := newMaintenanceSession(t, auth.RoleOwner, nil)

	require.NoError(t, dispatchMaintenance(s, "drain", ""))

	res := maintenanceResult(t, cs)
	assert.False(t, res.GetOk())
	assert.Equal(t, "unavailable", res.GetErrorCode())
}

// TestNodeMaintenance_UnknownActionRejected: only "drain" is supported.
func TestNodeMaintenance_UnknownActionRejected(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "ready"}
	s, cs := newMaintenanceSession(t, auth.RoleOwner, h)

	require.NoError(t, dispatchMaintenance(s, "explode", ""))

	res := maintenanceResult(t, cs)
	assert.False(t, res.GetOk())
	assert.Equal(t, "bad_request", res.GetErrorCode())
	assert.Equal(t, 0, h.beginCalls)
}

// TestNodeMaintenance_DefaultsToDrain: an empty action defaults to "drain"
// (the only action), so the trigger still funnels through.
func TestNodeMaintenance_DefaultsToDrain(t *testing.T) {
	h := &fakeMaintenanceHandler{nodeID: "bff-2", state: "draining"}
	s, cs := newMaintenanceSession(t, auth.RoleOwner, h)

	require.NoError(t, dispatchMaintenance(s, "", "boot"))

	res := maintenanceResult(t, cs)
	assert.True(t, res.GetOk())
	assert.Equal(t, 1, h.beginCalls)
}
