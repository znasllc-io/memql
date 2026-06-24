package memql

// Tests for the owner-gated durable-promote gRPC handler (issue
// znasllc-io/memql-cockpit#232, engine half). The handler validates a bundle and
// durably promotes its plain constructs into the SHARED registry, gated to the
// OWNER role only (strictly higher than the owner-or-developer authoring gate the
// validate / session-define handlers use).
//
// These tests cover the OWNER-ROLE GATE at the wire surface: a non-owner caller
// is refused with PermissionDenied BEFORE any engine work, and the gate runs
// ahead of the engine lookup (so an owner with no engine wired gets Unavailable,
// not a panic). The validate-fail + promote-success paths are covered at the
// engine level (component/memql/authoring_promote_*_test.go) where the store seam
// avoids a live DB.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func dispatchDurablePromote(s *streamSession, sources string) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m3",
		Payload: &memqlv1.MemqlClientMessage_DurablePromoteBundle{
			DurablePromoteBundle: &memqlv1.DurablePromoteBundleMsg{RequestId: "r3", Sources: sources},
		},
	}
	return s.handleDurablePromoteBundle(env, env.GetDurablePromoteBundle())
}

// The durable-promote gate rejects every NON-OWNER role (developer / admin /
// writer / reader) with PermissionDenied -- even developer, who CAN author +
// session-define but may NOT make a construct durable. This matches the MCP
// promote owner-only gate (roleCanPromote).
func TestDurablePromote_RejectsNonOwner(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleAdmin, auth.RoleWriter, auth.RoleReader} {
		t.Run(string(role), func(t *testing.T) {
			s, cs := newAuthoringSession(t, role, "u1")
			require.NoError(t, dispatchDurablePromote(s, authConceptSrc))

			msg := cs.lastSent()
			require.NotNil(t, msg)
			qe := msg.GetQueryError()
			require.NotNil(t, qe, "a non-owner caller must get a query error, got %T", msg.GetPayload())
			assert.Equal(t, "PermissionDenied", qe.GetError().GetCode())
		})
	}
}

// An OWNER passes the gate; with no engine wired on this session the handler
// returns a typed Unavailable (proving the gate ran first and did NOT deny the
// owner -- the refusal is the engine-absent one, not a permission denial).
func TestDurablePromote_OwnerPassesGate(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleOwner, "owner-1")
	require.NoError(t, dispatchDurablePromote(s, authConceptSrc))

	msg := cs.lastSent()
	require.NotNil(t, msg)
	qe := msg.GetQueryError()
	require.NotNil(t, qe, "owner with no engine should get an engine-unavailable error, got %T", msg.GetPayload())
	assert.Equal(t, "Unavailable", qe.GetError().GetCode(),
		"owner must pass the permission gate; the only refusal here is the absent engine")
}
