package memql

// Tests for the staged-tier gRPC handler (epic memql#3928).
//
// These cover the GATE at the wire surface, which is where staging differs from
// its two siblings and is the one thing a wire-level test can settle: staging
// takes the OWNER-OR-DEVELOPER bar (requireAuthoringRole, what session-define
// uses), not the owner-only bar the durable promote/demote handlers take.
//
// The reasoning, so a future reader does not "tighten" it: staging registers
// nothing shared and broadcasts nothing, so its blast radius is a database row
// rather than a change to what the cluster runs -- the same blast radius
// `define` already has, made durable. A developer who can author into a session
// can author into their own durable tier.
//
// The stage/refusal semantics themselves are covered at the engine level
// (component/memql/authoring_staged_test.go), where the store seam avoids a live
// DB.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func dispatchStageBundle(s *streamSession, sources string) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m9",
		Payload: &memqlv1.MemqlClientMessage_StageBundle{
			StageBundle: &memqlv1.StageBundleMsg{RequestId: "r9", Sources: sources},
		},
	}
	return s.handleStageBundle(env, env.GetStageBundle())
}

// A DEVELOPER passes the stage gate. With no engine wired the handler returns a
// typed Unavailable, which is what proves the gate ran and did not deny -- the
// only refusal left is the absent engine.
//
// This is the assertion that distinguishes staging from promoting: run the same
// role through dispatchDurablePromote and it is refused with PermissionDenied.
func TestStageBundle_DeveloperPassesGate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			s, cs := newAuthoringSession(t, role, "u1")
			require.NoError(t, dispatchStageBundle(s, authConceptSrc))

			msg := cs.lastSent()
			require.NotNil(t, msg)
			qe := msg.GetQueryError()
			require.NotNil(t, qe, "expected the engine-unavailable error, got %T", msg.GetPayload())
			assert.Equal(t, "Unavailable", qe.GetError().GetCode(),
				"%s must pass the staging gate -- staging is durable session-define, not promotion", role)
		})
	}
}

// Below the authoring bar the gate refuses, exactly as session-define does. A
// writer or reader cannot author, so they cannot stage.
func TestStageBundle_RejectsBelowTheAuthoringBar(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		t.Run(string(role), func(t *testing.T) {
			s, cs := newAuthoringSession(t, role, "u1")
			require.NoError(t, dispatchStageBundle(s, authConceptSrc))

			msg := cs.lastSent()
			require.NotNil(t, msg)
			qe := msg.GetQueryError()
			require.NotNil(t, qe, "a non-authoring caller must get a query error, got %T", msg.GetPayload())
			assert.Equal(t, "PermissionDenied", qe.GetError().GetCode())
		})
	}
}

// A nil body is refused with InvalidArgument before anything else, matching the
// order every other authoring handler checks in.
func TestStageBundle_RejectsMissingBody(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleOwner, "owner-1")
	env := &memqlv1.MemqlClientMessage{MessageId: "m9"}
	require.NoError(t, s.handleStageBundle(env, nil))

	msg := cs.lastSent()
	require.NotNil(t, msg)
	qe := msg.GetQueryError()
	require.NotNil(t, qe)
	assert.Equal(t, "InvalidArgument", qe.GetError().GetCode())
}
