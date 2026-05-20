package memql

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/worker"
)

// stubWorkerResolver is a fixed-result WorkerTokenResolver for tests.
type stubWorkerResolver struct {
	ident *worker.WorkerIdentity
	err   error
}

func (s *stubWorkerResolver) ResolveWorkerToken(_ context.Context, _ string) (*worker.WorkerIdentity, error) {
	return s.ident, s.err
}

func workerServiceInfo() *grpc.StreamServerInfo {
	return &grpc.StreamServerInfo{FullMethod: workerServicePathPrefix + "Stream"}
}

func nonWorkerServiceInfo() *grpc.StreamServerInfo {
	return &grpc.StreamServerInfo{FullMethod: "/znasllc.memql.v1.MemqlService/Stream"}
}

func TestWorkerAwareInterceptor_AdmitsActiveUnexpiredToken(t *testing.T) {
	resolver := &stubWorkerResolver{ident: &worker.WorkerIdentity{
		IdentityId:  "v1:identity:identity:wkr-1",
		OwnerUserId: "v1:identity:user:alice",
		Active:      true,
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}}
	cap := &captureHandler{}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_validtoken"), workerServiceInfo(), cap.handle)
	if err != nil {
		t.Fatalf("expected admit, got %v", err)
	}
	if !cap.called {
		t.Fatalf("handler was not invoked")
	}
	if !worker.IsWorkerSubject(cap.gotCtx) {
		t.Fatalf("worker identity not stamped on admitted context")
	}
}

func TestWorkerAwareInterceptor_RejectsRevokedToken(t *testing.T) {
	// Worker tokens are revoked by flipping Active=false (see
	// workertoken.Store.Revoke). The interceptor must reject those.
	resolver := &stubWorkerResolver{ident: &worker.WorkerIdentity{
		IdentityId:  "v1:identity:identity:wkr-1",
		OwnerUserId: "v1:identity:user:alice",
		Active:      false,
	}}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_revokedtoken"), workerServiceInfo(), nil)
	if got, want := status.Code(err), codes.Unauthenticated; got != want {
		t.Fatalf("revoked token: code=%v, want %v (err=%v)", got, want, err)
	}
}

func TestWorkerAwareInterceptor_RejectsExpiredToken(t *testing.T) {
	resolver := &stubWorkerResolver{ident: &worker.WorkerIdentity{
		IdentityId:  "v1:identity:identity:wkr-1",
		OwnerUserId: "v1:identity:user:alice",
		Active:      true,
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
	}}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_expiredtoken"), workerServiceInfo(), nil)
	if got, want := status.Code(err), codes.Unauthenticated; got != want {
		t.Fatalf("expired token: code=%v, want %v (err=%v)", got, want, err)
	}
}

func TestWorkerAwareInterceptor_AdmitsNonExpiringToken(t *testing.T) {
	// ExpiresAt zero value = non-expiring; must not be rejected as expired.
	resolver := &stubWorkerResolver{ident: &worker.WorkerIdentity{
		IdentityId:  "v1:identity:identity:wkr-1",
		OwnerUserId: "v1:identity:user:alice",
		Active:      true,
		// ExpiresAt left as zero value
	}}
	cap := &captureHandler{}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_nonexpiring"), workerServiceInfo(), cap.handle)
	if err != nil {
		t.Fatalf("non-expiring token: expected admit, got %v", err)
	}
	if !cap.called {
		t.Fatalf("handler was not invoked")
	}
}

func TestWorkerAwareInterceptor_RejectsWorkerTokenOnNonWorkerServicePath(t *testing.T) {
	// Surface-pinning: a worker token presented on MemqlService.Stream must
	// be rejected with PermissionDenied, even if the token itself is valid.
	resolver := &stubWorkerResolver{ident: &worker.WorkerIdentity{
		IdentityId:  "v1:identity:identity:wkr-1",
		OwnerUserId: "v1:identity:user:alice",
		Active:      true,
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_validtoken"), nonWorkerServiceInfo(), nil)
	if got, want := status.Code(err), codes.PermissionDenied; got != want {
		t.Fatalf("worker token on non-WorkerService: code=%v, want %v (err=%v)", got, want, err)
	}
}

func TestWorkerAwareInterceptor_RejectsBearerOnWorkerServicePath(t *testing.T) {
	// Inverse surface-pinning: a Bearer JWT on WorkerService must be rejected
	// at the interceptor level (only worker tokens may speak that surface).
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, nil, nil)

	err := intr(nil, streamWithAuthHeader("Bearer somejwt"), workerServiceInfo(), nil)
	if got, want := status.Code(err), codes.Unauthenticated; got != want {
		t.Fatalf("Bearer on WorkerService: code=%v, want %v (err=%v)", got, want, err)
	}
}

func TestWorkerAwareInterceptor_FallthroughOnNonWorkerSchemeAndNonWorkerPath(t *testing.T) {
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, nil, nil)

	err := intr(nil, streamWithAuthHeader("Bearer somejwt"), nonWorkerServiceInfo(), nil)
	if !errors.Is(err, sentinelFallthrough) {
		t.Fatalf("expected fallthrough, got %v", err)
	}
}

func TestWorkerAwareInterceptor_RejectsResolverError(t *testing.T) {
	resolver := &stubWorkerResolver{err: ErrWorkerTokenNotFound}
	intr := NewWorkerAwareStreamInterceptor(fallthroughInterceptor, resolver, nil)

	err := intr(nil, streamWithAuthHeader("Worker mql_wkr_unknown"), workerServiceInfo(), nil)
	if got, want := status.Code(err), codes.Unauthenticated; got != want {
		t.Fatalf("unknown token: code=%v, want %v (err=%v)", got, want, err)
	}
}
