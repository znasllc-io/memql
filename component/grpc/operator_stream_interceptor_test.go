package memql

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/visionarys-io/memql/component/auth"
)

// fakeServerStream is a minimal grpc.ServerStream for interceptor tests.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

// streamWithAuthHeader builds an incoming context carrying the given
// Authorization header value.
func streamWithAuthHeader(header string) grpc.ServerStream {
	md := metadata.Pairs("authorization", header)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	return &fakeServerStream{ctx: ctx}
}

// captureHandler records the handler-side context so tests can
// assert what the interceptor stamped onto the stream.
type captureHandler struct {
	gotCtx context.Context
	called bool
}

func (c *captureHandler) handle(_ interface{}, ss grpc.ServerStream) error {
	c.called = true
	c.gotCtx = ss.Context()
	return nil
}

// fallthroughInterceptor records that the wrapped chain was invoked
// (i.e. operator-aware fell through). Returns sentinelErr so tests
// can distinguish "passed through" from "admitted via operator path".
var sentinelFallthrough = errors.New("fallthrough")

func fallthroughInterceptor(_ interface{}, _ grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
	return sentinelFallthrough
}

func TestOperatorAwareInterceptor_FallthroughOnNonOperatorScheme(t *testing.T) {
	// Master key set, but caller used Bearer -> hand off to base.
	t.Setenv(envMasterKey, "deadbeef")
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Bearer xyz"), &grpc.StreamServerInfo{}, nil)
	if !errors.Is(err, sentinelFallthrough) {
		t.Fatalf("expected fallthrough, got %v", err)
	}
}

func TestOperatorAwareInterceptor_FallthroughOnMissingHeader(t *testing.T) {
	t.Setenv(envMasterKey, "deadbeef")
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	// No authorization header at all.
	ss := &fakeServerStream{ctx: context.Background()}
	err := intr(nil, ss, &grpc.StreamServerInfo{}, nil)
	if !errors.Is(err, sentinelFallthrough) {
		t.Fatalf("expected fallthrough, got %v", err)
	}
}

func TestOperatorAwareInterceptor_AdmitsCorrectKey(t *testing.T) {
	t.Setenv(envMasterKey, "the-master-key")
	cap := &captureHandler{}
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator the-master-key"), &grpc.StreamServerInfo{}, cap.handle)
	if err != nil {
		t.Fatalf("expected admit, got %v", err)
	}
	if !cap.called {
		t.Fatalf("handler was not invoked")
	}
	claims, ok := auth.ClaimsFromContext(cap.gotCtx)
	if !ok {
		t.Fatalf("claims not stamped on admitted context")
	}
	if got, _ := claims["sub"].(string); got != OperatorSubject {
		t.Fatalf("sub claim=%q, want %q", got, OperatorSubject)
	}
	if got, _ := claims["role"].(string); got != "owner" {
		t.Fatalf("role claim=%q, want owner", got)
	}
	if !IsOperatorContext(cap.gotCtx) {
		t.Fatalf("IsOperatorContext returned false on admitted ctx")
	}
}

func TestOperatorAwareInterceptor_RejectsWrongKey(t *testing.T) {
	t.Setenv(envMasterKey, "the-master-key")
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator wrong-key"), &grpc.StreamServerInfo{}, nil)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestOperatorAwareInterceptor_RejectsWhenMasterKeyUnset(t *testing.T) {
	// Operator scheme presented but cluster has no operator key
	// configured -- fail closed.
	t.Setenv(envMasterKey, "")
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator anything"), &grpc.StreamServerInfo{}, nil)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestIsOperatorContext_FalseOnPlainContext(t *testing.T) {
	if IsOperatorContext(context.Background()) {
		t.Fatalf("IsOperatorContext should be false on plain context")
	}
	if IsOperatorContext(nil) {
		t.Fatalf("IsOperatorContext should be false on nil context")
	}
}
