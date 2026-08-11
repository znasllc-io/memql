package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/secret"
)

// testOperatorKey is a fixture of realistic LENGTH -- the interceptor refuses
// anything shorter than minOperatorKeyLen, so a toy value like "deadbeef"
// would now be rejected as a placeholder rather than admitted.
//
// It is CONSTRUCTED rather than written as a 64-character literal, on purpose.
// A literal of that shape reads as high-entropy to gitleaks' generic-api-key
// rule and would have added another entry to the allowlist treadmill
// memql#3484 documents. Not writing the literal is strictly better than
// suppressing it afterwards.
var (
	testOperatorKey = strings.Repeat("memqlspike-", 6)
	testMasterKey   = strings.Repeat("notacredential-", 5)
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
	// Operator key set, but caller used Bearer -> hand off to base.
	t.Setenv(auth.EnvOperatorKey, testOperatorKey)
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Bearer xyz"), &grpc.StreamServerInfo{}, nil)
	if !errors.Is(err, sentinelFallthrough) {
		t.Fatalf("expected fallthrough, got %v", err)
	}
}

func TestOperatorAwareInterceptor_FallthroughOnMissingHeader(t *testing.T) {
	t.Setenv(auth.EnvOperatorKey, testOperatorKey)
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	// No authorization header at all.
	ss := &fakeServerStream{ctx: context.Background()}
	err := intr(nil, ss, &grpc.StreamServerInfo{}, nil)
	if !errors.Is(err, sentinelFallthrough) {
		t.Fatalf("expected fallthrough, got %v", err)
	}
}

func TestOperatorAwareInterceptor_AdmitsCorrectKey(t *testing.T) {
	t.Setenv(auth.EnvOperatorKey, testOperatorKey)
	cap := &captureHandler{}
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator "+testOperatorKey), &grpc.StreamServerInfo{}, cap.handle)
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
	t.Setenv(auth.EnvOperatorKey, testOperatorKey)
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator wrong-key"), &grpc.StreamServerInfo{}, nil)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestOperatorAwareInterceptor_RejectsWhenOperatorKeyUnset(t *testing.T) {
	// Operator scheme presented but cluster has no operator key
	// configured -- fail closed.
	t.Setenv(auth.EnvOperatorKey, "")
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

// TestOperatorAwareInterceptor_RejectsMasterKey is the memql#3519 regression,
// and the single most important assertion in this file.
//
// The operator path used to authenticate against MEMQL_MASTER_KEY. That made
// the envelope-decryption key a cluster-owner bearer token over the network --
// while the installer wrote it into a world-readable ~/.bashrc by default and
// ESO delivered it to production pods. Splitting the credentials is only
// meaningful if the old one STOPS WORKING, so this test sets the master key to
// a valid-shaped value, presents it as the operator credential, and requires a
// rejection.
//
// If someone reintroduces a fallback to MEMQL_MASTER_KEY "for compatibility",
// this test is what fails.
func TestOperatorAwareInterceptor_RejectsMasterKey(t *testing.T) {
	masterKey := testMasterKey
	t.Setenv(secret.EnvMasterKey, masterKey)
	t.Setenv(auth.EnvOperatorKey, testOperatorKey)

	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator "+masterKey), &grpc.StreamServerInfo{}, nil)
	if err == nil {
		t.Fatalf("master key was accepted as the operator credential; the memql#3519 split is not in force")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

// TestOperatorAwareInterceptor_RejectsMasterKeyWhenOperatorKeyUnset covers the
// deployment that upgrades WITHOUT seeding MEMQL_OPERATOR_KEY. The failure mode
// there must be "operator tooling stops working", not "the master key still
// works" -- otherwise the split silently does nothing on every cluster that has
// not been reconfigured yet.
func TestOperatorAwareInterceptor_RejectsMasterKeyWhenOperatorKeyUnset(t *testing.T) {
	masterKey := testMasterKey
	t.Setenv(secret.EnvMasterKey, masterKey)
	t.Setenv(auth.EnvOperatorKey, "")

	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator "+masterKey), &grpc.StreamServerInfo{}, nil)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

// TestOperatorAwareInterceptor_RejectsShortConfiguredKey pins the floor on
// operator error: a placeholder somebody meant to replace must not become a
// cluster-owner credential just because it is non-empty. Refused exactly like
// an unset key.
func TestOperatorAwareInterceptor_RejectsShortConfiguredKey(t *testing.T) {
	t.Setenv(auth.EnvOperatorKey, "test")
	intr := NewOperatorAwareStreamInterceptor(fallthroughInterceptor, nil)
	err := intr(nil, streamWithAuthHeader("Operator test"), &grpc.StreamServerInfo{}, nil)
	if err == nil {
		t.Fatalf("a 4-character operator key was accepted")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}
