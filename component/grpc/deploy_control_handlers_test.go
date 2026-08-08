package memql

// Tests for handleDeployControl -- the stream landing for the bridged
// DeployControlService surface (memql#3311).
//
// The role matrix itself is tested where it lives, against the real service,
// in component/deploycontrol/stream_parity_test.go (streamed vs unary, every
// RPC, every role). What is under test HERE is the wiring between the two:
// that the handler stamps the session's identity onto the context the service
// gate reads, that a reply comes back on the stream instead of an error that
// would tear it down, and that a node without the service says so rather than
// answering an empty success.

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// gateProbe stands in for the deploy-control service and records the auth
// context the handler handed it. That context is the ENTIRE contract between
// the stream layer and the gate -- if it does not arrive, the service's
// resolveActor fails closed and every bridged call is Unauthenticated, so the
// bridge would look secure while being completely broken.
type gateProbe struct {
	memqlv1.UnimplementedDeployControlServiceServer
	sawAccess *auth.AccessContext
	// denyWith, when set, is returned instead of a result -- standing in for
	// the real gate's PermissionDenied so the handler's error path is
	// exercised without reconstructing the service.
	denyWith error
}

func (g *gateProbe) Promote(ctx context.Context, _ *memqlv1.PromoteRequest) (*memqlv1.ActionResult, error) {
	if ac, ok := auth.AccessFromContext(ctx); ok {
		g.sawAccess = ac
	}
	if g.denyWith != nil {
		return nil, g.denyWith
	}
	return &memqlv1.ActionResult{Ok: true, AuditEventId: "aud-1", Message: "promoted"}, nil
}

func newDeployControlSession(t *testing.T, role auth.Role, h memqlv1.DeployControlServiceServer) (*streamSession, *captureStream) {
	t.Helper()
	cs := newCaptureStream(t)
	svc := &service{
		logger:               slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		deployControlHandler: h,
	}
	s := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		access:  &auth.AccessContext{UserId: "u1", PrimaryEmail: "op@example.com", Role: role},
	}
	s.accessLoaded = true // short-circuit ensureAccess to the seeded access
	return s, cs
}

func promoteEnvelope() *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_DeployControl{
			DeployControl: &memqlv1.DeployControlMsg{
				RequestId: "r1",
				Request: &memqlv1.DeployControlMsg_Promote{
					Promote: &memqlv1.PromoteRequest{Version: "1.2.3"},
				},
			},
		},
	}
}

func deployControlResult(t *testing.T, cs *captureStream) *memqlv1.DeployControlResult {
	t.Helper()
	msg := cs.lastSent()
	require.NotNil(t, msg, "handler must send a reply")
	res := msg.GetDeployControlResult()
	require.NotNil(t, res, "reply must be a DeployControlResult")
	return res
}

// The identity the gate sees must be the STREAM's identity. A bridged call
// that arrived with no actor -- or with someone else's -- is the whole
// privilege-escalation risk of adding a second surface.
func TestDeployControl_StampsTheSessionIdentityForTheGate(t *testing.T) {
	probe := &gateProbe{}
	s, cs := newDeployControlSession(t, auth.RoleOwner, probe)

	env := promoteEnvelope()
	require.NoError(t, s.handleDeployControl(env, env.GetDeployControl()))

	require.NotNil(t, probe.sawAccess, "the service must receive the caller's AccessContext")
	assert.Equal(t, "u1", probe.sawAccess.UserId)
	assert.Equal(t, auth.RoleOwner, probe.sawAccess.Role)

	res := deployControlResult(t, cs)
	assert.True(t, res.GetOk())
	assert.Equal(t, "r1", res.GetRequestId(), "request_id must round-trip for correlation")
	require.NotNil(t, res.GetAction())
	assert.Equal(t, "aud-1", res.GetAction().GetAuditEventId(),
		"the audit id must survive the bridge -- the console shows it to the operator")
}

// A refusal rides IN the reply. Returning a Go error here would kill the
// multiplexed stream and every other in-flight request on it, so the handler
// must always answer with an envelope carrying the gRPC code.
func TestDeployControl_DenialRidesInTheReplyNotAsAStreamError(t *testing.T) {
	probe := &gateProbe{denyWith: status.Error(codes.PermissionDenied, "deploy console: promote requires owner or admin role")}
	s, cs := newDeployControlSession(t, auth.RoleReader, probe)

	env := promoteEnvelope()
	assert.NoError(t, s.handleDeployControl(env, env.GetDeployControl()),
		"a denial must not surface as a handler error (it would tear down the stream)")

	res := deployControlResult(t, cs)
	assert.False(t, res.GetOk())
	assert.Equal(t, int32(codes.PermissionDenied), res.GetErrorCode())
	assert.Contains(t, res.GetErrorMessage(), "requires owner or admin")
	assert.Nil(t, res.GetResult(), "a denial must carry no result")
}

// Every node type runs the same engine image, but only the identity node
// hosts the deploy-control service. A node without it must say Unimplemented
// rather than answer an empty success a client would read as "it ran".
func TestDeployControl_UnimplementedWhenTheNodeHasNoService(t *testing.T) {
	s, cs := newDeployControlSession(t, auth.RoleOwner, nil)

	env := promoteEnvelope()
	require.NoError(t, s.handleDeployControl(env, env.GetDeployControl()))

	res := deployControlResult(t, cs)
	assert.False(t, res.GetOk())
	assert.Equal(t, int32(codes.Unimplemented), res.GetErrorCode())
}

// The payload must be reachable through the stream's payload switch, not just
// by calling the handler directly -- a message wired into the proto but not
// into handleMessage is silently ignored.
func TestDeployControl_ReachableThroughTheStreamPayloadSwitch(t *testing.T) {
	probe := &gateProbe{}
	s, cs := newDeployControlSession(t, auth.RoleOwner, probe)

	require.NoError(t, s.handleMessage(promoteEnvelope()))

	res := deployControlResult(t, cs)
	assert.True(t, res.GetOk(), "handleMessage must route DeployControl to its handler")
}

// A live badge grant is attribution-grade, not a full operator session: it
// must not be able to promote a release, exactly as it cannot drain a node.
func TestDeployControl_RestrictedUnderALiveBadgeGrant(t *testing.T) {
	s, _ := newDeployControlSession(t, auth.RoleOwner, &gateProbe{})
	// Stamp a live grant directly: badgeStamped short-circuits the lazy
	// claims read, which the capture stream's bare context cannot satisfy.
	s.badgeStamped = true
	s.badgeExpiresAt = time.Now().Add(time.Hour)

	assert.Equal(t, badgeGateRestricted, s.badgeGate(promoteEnvelope()),
		"deploy control must be pinned away from badge sessions")
}
