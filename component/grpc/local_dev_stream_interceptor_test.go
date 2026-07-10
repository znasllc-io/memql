package memql

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/znasllc-io/memql/component/auth"
)

// TestLocalDevInterceptor_StampsOwnerIdentity verifies the no-auth
// troubleshooting path admits every stream as a synthetic local-dev
// cluster owner, so downstream per-row authz resolves to an owner
// AccessContext via auth.FallbackFromClaims.
func TestLocalDevInterceptor_StampsOwnerIdentity(t *testing.T) {
	intr := NewLocalDevStreamInterceptor(nil)
	cap := &captureHandler{}
	ss := &fakeServerStream{ctx: context.Background()}

	if err := intr(nil, ss, &grpc.StreamServerInfo{FullMethod: "/x/y"}, cap.handle); err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if !cap.called {
		t.Fatal("handler was not invoked; local-dev interceptor must never reject")
	}

	// The stamped context must resolve to the local-dev subject.
	id, err := auth.UserIdentityFromContext(cap.gotCtx)
	if err != nil {
		t.Fatalf("UserIdentityFromContext: %v", err)
	}
	if id.Subject != LocalDevSubject {
		t.Fatalf("subject = %q, want %q", id.Subject, LocalDevSubject)
	}

	// The stamped claims must yield an owner AccessContext through the
	// same fallback ensureAccess uses (LoadFromClaims rejects the
	// non-provisioned subject, so FallbackFromClaims is the live path).
	claims, ok := auth.ClaimsFromContext(cap.gotCtx)
	if !ok {
		t.Fatal("no claims stamped on context")
	}
	ac := auth.FallbackFromClaims(claims)
	if ac == nil || !ac.IsClusterOwner() {
		t.Fatalf("expected cluster-owner AccessContext, got %+v", ac)
	}
	if ac.UserId != LocalDevSubject {
		t.Fatalf("UserId = %q, want %q", ac.UserId, LocalDevSubject)
	}
}
