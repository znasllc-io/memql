package memql

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestWorkIntakeKeepsRotatedBadgeCeilingAndExpiry(t *testing.T) {
	owner := "v1:identity:user:alice"
	expires := time.Now().Add(time.Minute)
	// Stream-open claims are deliberately older/more privileged than the live grant.
	streamCtx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner, Claims: map[string]any{"role": "owner"}})
	access := &auth.AccessContext{UserId: owner, Role: auth.RoleReader}
	session := &streamSession{service: &service{logger: testLogger()}, stream: newRecordingClientStream(streamCtx), logger: testLogger(), access: access, accessLoaded: true, badgeStamped: true, credentialClass: auth.ForwardedClassBadge, credentialCeiling: string(auth.RoleReader), badgeExpiresAt: expires}
	ctx := session.bindQueryAuthority(auth.ContextWithAccess(streamCtx, access))
	grant, err := auth.CaptureExecutionAuthority(ctx)
	if err != nil || grant["roleCeiling"] != "reader" || grant["credentialClass"] != auth.ForwardedClassBadge || grant["expiresAt"] != expires.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("lost live badge restriction: %+v %v", grant, err)
	}
	session.badgeExpiresAt = time.Now().Add(-time.Second)
	if _, err = auth.CaptureExecutionAuthority(session.bindQueryAuthority(auth.ContextWithAccess(streamCtx, access))); err == nil {
		t.Fatal("expired badge opened durable work")
	}
}

func TestQueryModelAuthoritySurvivesRotationAndExpiresAtReceiver(t *testing.T) {
	_, issueBadge, verifier := newRotateAuthFixtureWithBadge(t)
	session, stream := newSessionForRotate(&service{verifier: verifier})
	badge := issueBadge("v1:identity:user:operator-query", "reader", 5*time.Minute)
	if err := session.handleRotateAuth(&memqlv1.MemqlClientMessage{MessageId: "rotate-query"}, &memqlv1.RotateAuthMsg{AccessToken: badge}); err != nil {
		t.Fatal(err)
	}
	if result := stream.lastSent().GetRotateAuthResult(); result == nil || !result.GetOk() {
		t.Fatalf("rotation failed: %+v", result)
	}
	ctx := auth.ContextWithAccess(session.stream.Context(), session.ensureAccess(session.stream.Context()))
	ctx = session.bindQueryAuthority(ctx)
	assertion, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok || assertion.CredentialClass != auth.ForwardedClassBadge || assertion.RoleCeiling != auth.RoleReader {
		t.Fatalf("query model call lost the rotated badge: %+v", assertion)
	}
	// The model receiver sees only the assertion; none of the BFF's session
	// state or pre-rotation claims are available on that replica.
	receiver, err := auth.VerifyForwardedAuthority(assertion, time.Now())
	if err != nil || receiver.Role != auth.RoleReader || receiver.IsClusterOwner() {
		t.Fatalf("receiver broadened query authority: %+v %v", receiver, err)
	}
	if _, err := auth.VerifyForwardedAuthority(assertion, assertion.ExpiresAt.Add(time.Second)); err == nil {
		t.Fatal("receiver accepted an expired query badge")
	}
	// A query actor resolved before a concurrent rotation cannot borrow the
	// new grant. Nor can a failed bind leave an older parent assertion usable.
	stale := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: assertion.Subject, Role: auth.RoleOwner})
	stale = session.bindQueryAuthority(stale)
	rejected, _ := auth.ForwardedAuthorityFromContext(stale)
	if _, err := auth.VerifyForwardedAuthority(rejected, time.Now()); err == nil {
		t.Fatal("mismatched query actor retained a usable forwarding assertion")
	}
	if _, err := auth.CaptureExecutionAuthority(stale); err == nil {
		t.Fatal("mismatched query actor opened durable work")
	}
}
