package memql

import (
	"context"
	"github.com/znasllc-io/memql/component/auth"
	"testing"
	"time"
)

func TestWorkIntakeKeepsRotatedBadgeCeilingAndExpiry(t *testing.T) {
	owner := "v1:identity:user:alice"
	expires := time.Now().Add(time.Minute)
	// Stream-open claims are deliberately older/more privileged than the live grant.
	streamCtx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner, Claims: map[string]any{"role": "owner"}})
	access := &auth.AccessContext{UserId: owner, Role: auth.RoleReader}
	session := &streamSession{service: &service{logger: testLogger()}, stream: newRecordingClientStream(streamCtx), logger: testLogger(), access: access, accessLoaded: true, badgeStamped: true, credentialClass: auth.ForwardedClassBadge, credentialCeiling: string(auth.RoleReader), badgeExpiresAt: expires}
	ctx := session.bindWorkIntakeAuthority(auth.ContextWithAccess(streamCtx, access))
	grant, err := auth.CaptureExecutionAuthority(ctx)
	if err != nil || grant["roleCeiling"] != "reader" || grant["credentialClass"] != auth.ForwardedClassBadge || grant["expiresAt"] != expires.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("lost live badge restriction: %+v %v", grant, err)
	}
	session.badgeExpiresAt = time.Now().Add(-time.Second)
	if _, err = auth.CaptureExecutionAuthority(session.bindWorkIntakeAuthority(auth.ContextWithAccess(streamCtx, access))); err == nil {
		t.Fatal("expired badge opened durable work")
	}
}
