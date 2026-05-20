package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPanicRecoveryStreamInterceptor_CatchesPanic asserts the central
// invariant: a panic in the downstream handler is caught, never reaches
// the client raw, and produces a codes.Internal + ERR-* trace id.
func TestPanicRecoveryStreamInterceptor_CatchesPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	interceptor := NewPanicRecoveryStreamInterceptor(nil, logger)

	handler := func(_ interface{}, _ grpc.ServerStream) error {
		panic("SECRET-CANARY-do-not-leak")
	}

	err := interceptor(nil, &fakeServerStream{}, &grpc.StreamServerInfo{FullMethod: "/test/Boom"}, handler)

	if err == nil {
		t.Fatalf("expected an error from the recovered panic")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a status: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Fatalf("code = %v, want Internal", st.Code())
	}
	if strings.Contains(st.Message(), "SECRET-CANARY") {
		t.Fatalf("message leaks panic value: %q", st.Message())
	}
	if !strings.Contains(st.Message(), "ERR-") {
		t.Errorf("message lacks ERR-* trace id: %q", st.Message())
	}
}

// TestPanicRecoveryStreamInterceptor_PassesThroughNonPanic asserts the
// interceptor doesn't disturb normal handler returns -- the wrapped
// handler's err (or nil) is propagated unchanged.
func TestPanicRecoveryStreamInterceptor_PassesThroughNonPanic(t *testing.T) {
	interceptor := NewPanicRecoveryStreamInterceptor(nil, nil)

	t.Run("nil err", func(t *testing.T) {
		handler := func(_ interface{}, _ grpc.ServerStream) error { return nil }
		if err := interceptor(nil, &fakeServerStream{}, &grpc.StreamServerInfo{}, handler); err != nil {
			t.Fatalf("expected nil err, got %v", err)
		}
	})

	t.Run("real err propagates", func(t *testing.T) {
		sentinel := status.Error(codes.NotFound, "not found")
		handler := func(_ interface{}, _ grpc.ServerStream) error { return sentinel }
		got := interceptor(nil, &fakeServerStream{}, &grpc.StreamServerInfo{}, handler)
		if got != sentinel {
			t.Fatalf("expected sentinel, got %v", got)
		}
	})
}

// TestPanicRecoveryStreamInterceptor_WrapsBaseInterceptor confirms that
// when a `base` interceptor is supplied, the panic recovery sits AROUND
// it -- a panic in the base interceptor itself is still caught.
func TestPanicRecoveryStreamInterceptor_WrapsBaseInterceptor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A "base" interceptor that itself panics before reaching the handler.
	base := func(_ interface{}, _ grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
		panic("base-layer panic")
	}

	wrapped := NewPanicRecoveryStreamInterceptor(base, logger)

	handler := func(_ interface{}, _ grpc.ServerStream) error { return nil }
	err := wrapped(nil, &fakeServerStream{}, &grpc.StreamServerInfo{FullMethod: "/test/BasePanic"}, handler)

	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Fatalf("code = %v, want Internal", st.Code())
	}
	if strings.Contains(st.Message(), "base-layer panic") {
		t.Fatalf("message leaks panic value: %q", st.Message())
	}
}
