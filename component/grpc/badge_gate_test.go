package memql

// Badge expiry gate tests (memql#2513): an expired class="badge"
// operator grant must stop doing work mid-stream (rotation is durable
// and session revocation is stream-open-only), while control frames
// stay admitted so the client can rotate back to the terminal bearer.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func badgeStreamClaims(class string, exp time.Time) map[string]any {
	claims := map[string]any{
		"sub":   "v1:identity:user:op-1",
		"class": class,
	}
	if !exp.IsZero() {
		claims["exp"] = float64(exp.Unix())
	}
	return claims
}

func newBadgeGateSession(t *testing.T, claims map[string]any) (*streamSession, *recordingClientStream) {
	t.Helper()
	ctx := auth.ContextWithClaims(context.Background(), claims)
	stream := newRecordingClientStream(ctx)
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return newTestStreamSession(svc, stream), stream
}

func executeQueryEnvelope(id string) *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{
		MessageId: id,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: id, Query: "concept==v1:demo"},
		},
	}
}

func TestBadgeGate_ExpiredGrantRejectsWork(t *testing.T) {
	s, stream := newBadgeGateSession(t, badgeStreamClaims("badge", time.Now().Add(-time.Minute)))

	require.NoError(t, s.handleMessage(executeQueryEnvelope("q1")))

	msgs := stream.snapshot()
	require.Len(t, msgs, 1)
	qe := msgs[0].GetQueryError()
	require.NotNil(t, qe, "expired badge grant must yield a QueryError, got %v", msgs[0])
	require.Contains(t, qe.GetError().GetMessage(), "badge_grant_expired")
}

func TestBadgeGate_LiveGrantPassesWork(t *testing.T) {
	s, stream := newBadgeGateSession(t, badgeStreamClaims("badge", time.Now().Add(time.Hour)))

	// The stub service has no engine, so the query fails downstream --
	// what matters is that it is NOT the gate's typed rejection.
	require.NoError(t, s.handleMessage(executeQueryEnvelope("q1")))
	for _, m := range stream.snapshot() {
		if qe := m.GetQueryError(); qe != nil {
			require.NotContains(t, qe.GetError().GetMessage(), "badge_grant_expired")
		}
	}
}

func TestBadgeGate_NonBadgeClassNeverGated(t *testing.T) {
	s, stream := newBadgeGateSession(t, badgeStreamClaims("", time.Now().Add(-time.Hour)))

	require.NoError(t, s.handleMessage(executeQueryEnvelope("q1")))
	for _, m := range stream.snapshot() {
		if qe := m.GetQueryError(); qe != nil {
			require.NotContains(t, qe.GetError().GetMessage(), "badge_grant_expired")
		}
	}
}

func TestBadgeGate_ControlFramesStayAdmitted(t *testing.T) {
	// Even with an expired badge grant, RotateAuth must reach its
	// handler (here: it replies verifier_unconfigured from the stub
	// service -- proof it passed the gate) so the kiosk can rotate
	// back to the terminal bearer.
	s, stream := newBadgeGateSession(t, badgeStreamClaims("badge", time.Now().Add(-time.Minute)))

	rotate := &memqlv1.MemqlClientMessage{
		MessageId: "r1",
		Payload: &memqlv1.MemqlClientMessage_RotateAuth{
			RotateAuth: &memqlv1.RotateAuthMsg{AccessToken: "whatever"},
		},
	}
	require.NoError(t, s.handleMessage(rotate))

	msgs := stream.snapshot()
	require.Len(t, msgs, 1)
	rr := msgs[0].GetRotateAuthResult()
	require.NotNil(t, rr, "RotateAuth must pass the gate; got %v", msgs[0])
	require.False(t, rr.GetOk())
	require.Equal(t, "verifier_unconfigured", rr.GetError())

	// ClientHello likewise.
	hello := &memqlv1.MemqlClientMessage{
		MessageId: "h1",
		Payload:   &memqlv1.MemqlClientMessage_ClientHello{ClientHello: &memqlv1.ClientHello{}},
	}
	require.NoError(t, s.handleMessage(hello))
	msgs = stream.snapshot()
	require.Len(t, msgs, 2)
	require.NotNil(t, msgs[1].GetServerHello())
}

func TestStampBadgeExpiryLocked_RotationRearmsAndDisarms(t *testing.T) {
	s, _ := newBadgeGateSession(t, badgeStreamClaims("", time.Time{}))

	// Rotating IN a badge grant arms the gate at its exp.
	exp := time.Now().Add(30 * time.Second)
	s.accessMu.Lock()
	s.stampBadgeExpiryLocked("badge", exp)
	s.accessMu.Unlock()
	require.True(t, s.badgeGateAllows(executeQueryEnvelope("q1")), "grant still live")

	s.accessMu.Lock()
	s.stampBadgeExpiryLocked("badge", time.Now().Add(-time.Second))
	s.accessMu.Unlock()
	require.False(t, s.badgeGateAllows(executeQueryEnvelope("q2")), "grant expired")

	// Rotating back to a user-class bearer disarms the gate.
	s.accessMu.Lock()
	s.stampBadgeExpiryLocked("user", time.Time{})
	s.accessMu.Unlock()
	require.True(t, s.badgeGateAllows(executeQueryEnvelope("q3")))
}

func TestBadgeExpiryFromClaims(t *testing.T) {
	exp := time.Now().Add(time.Minute).Truncate(time.Second)
	got := badgeExpiryFromClaims(map[string]any{"class": "badge", "exp": float64(exp.Unix())})
	require.True(t, got.Equal(exp), "got %v want %v", got, exp)

	require.True(t, badgeExpiryFromClaims(map[string]any{"class": "user", "exp": float64(exp.Unix())}).IsZero())
	require.True(t, badgeExpiryFromClaims(map[string]any{"class": "badge"}).IsZero(),
		"missing exp disables the gate rather than bricking the stream")
	require.True(t, badgeExpiryFromClaims(nil).IsZero())
}

func TestBadgeGate_LiveGrantRestrictedFromCredentialSurfaces(t *testing.T) {
	// A LIVE badge grant is attribution-grade, not a full user session:
	// credential/session management must be rejected even before exp,
	// so a walked-away kiosk can never mint the operator a durable
	// credential that outlives the grant's TTL containment.
	s, stream := newBadgeGateSession(t, badgeStreamClaims("badge", time.Now().Add(time.Hour)))

	mint := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_CreateWorkerToken{
			CreateWorkerToken: &memqlv1.CreateWorkerTokenMsg{RequestId: "m1", Name: "sneaky"},
		},
	}
	require.NoError(t, s.handleMessage(mint))

	msgs := stream.snapshot()
	require.Len(t, msgs, 1)
	qe := msgs[0].GetQueryError()
	require.NotNil(t, qe, "credential mint under a badge grant must be rejected, got %v", msgs[0])
	require.Contains(t, qe.GetError().GetMessage(), "badge_grant_restricted")

	// Ordinary work still passes while the grant is live.
	require.NoError(t, s.handleMessage(executeQueryEnvelope("q1")))
	for _, m := range stream.snapshot()[1:] {
		if e := m.GetQueryError(); e != nil {
			require.NotContains(t, e.GetError().GetMessage(), "badge_grant_restricted")
		}
	}
}
