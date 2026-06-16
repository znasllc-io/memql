package verifier_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/metrics"
)

// TestStreamInterceptor_UnknownKid_IncrementsAuthRejectMetric proves the
// JWKS-skew symptom (#1523) bumps the unknown_kid auth-reject counter,
// so the staging alert can page on a node-auth reject storm instead of
// it staying a silent WARN line. JWKS serves issuer B's keys while the
// token is minted by issuer A, so the token's kid is absent from the
// keyset -> ErrUnknownKID -> ReasonUnknownKID.
func TestStreamInterceptor_UnknownKid_IncrementsAuthRejectMetric(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	_, issA := newIdentityIssuer(t, srv.URL)
	kmB, _ := newIdentityIssuer(t, srv.URL)
	// JWKS serves issuer B's keys; token below is minted by issuer A,
	// so A's kid is unknown to the served keyset.
	mux.Handle("/.well-known/jwks.json", jwksHandler(kmB))

	tok, _, err := issA.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:carol",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)

	before := metrics.AuthRejectValue(metrics.SurfaceGRPC, metrics.ReasonUnknownKID, metrics.CodeUnauthenticated)

	intercept := verifier.StreamInterceptor(v, slog.Default())
	handlerCalled := false
	ierr := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error {
			handlerCalled = true
			return nil
		})

	require.Error(t, ierr)
	assert.Equal(t, codes.Unauthenticated, status.Code(ierr))
	assert.False(t, handlerCalled, "handler must NOT run when auth rejects")

	after := metrics.AuthRejectValue(metrics.SurfaceGRPC, metrics.ReasonUnknownKID, metrics.CodeUnauthenticated)
	assert.Equal(t, float64(1), after-before, "unknown_kid auth-reject counter must increment by exactly 1")
}

// TestStreamInterceptor_MissingToken_IncrementsAuthRejectMetric proves a
// missing Authorization header bumps the missing_token bucket -- the
// generic Unauthenticated storm half of the same incident.
func TestStreamInterceptor_MissingToken_IncrementsAuthRejectMetric(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, _ := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))
	v := newVerifier(t, srv.URL)

	before := metrics.AuthRejectValue(metrics.SurfaceGRPC, metrics.ReasonMissingToken, metrics.CodeUnauthenticated)

	intercept := verifier.StreamInterceptor(v, slog.Default())
	ierr := intercept(nil, &fakeStream{ctx: context.Background()}, // no token metadata
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error { return nil })

	require.Error(t, ierr)
	assert.Equal(t, codes.Unauthenticated, status.Code(ierr))

	after := metrics.AuthRejectValue(metrics.SurfaceGRPC, metrics.ReasonMissingToken, metrics.CodeUnauthenticated)
	assert.Equal(t, float64(1), after-before, "missing_token auth-reject counter must increment by exactly 1")
}
