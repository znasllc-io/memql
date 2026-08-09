package verifier_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// reject_message_test.go -- memql#3400.
//
// The gRPC interceptor answered EVERY VerifyBearer failure with the single
// string "invalid or expired token". For the unknown-kid case that sentence is
// false in both halves: the token's signature is well-formed and its exp is in
// the future -- what is wrong is that this node's JWKS has no key with the
// token's kid, because the issuing replica minted its own.
//
// The cost is not cosmetic. An operator reading "invalid or expired token"
// goes looking at token lifetime, and there is a real open bug about exactly
// that (memql#3385) for the message to impersonate. The verifier has carried a
// typed ErrUnknownKID since memql#1523 -- it fed the metrics label but never
// the sentence the operator actually reads.

// TestStreamInterceptor_UnknownKid_SaysSo pins that a rejection caused by an
// unknown kid names that cause on the wire.
func TestStreamInterceptor_UnknownKid_SaysSo(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	// Two independent issuers: JWKS publishes B's keys, the token is minted
	// by A. This is exactly the two-identity-replicas topology of memql#3400,
	// reduced to one process.
	_, issA := newIdentityIssuer(t, srv.URL)
	kmB, _ := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(kmB))

	tok, _, err := issA.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:dana",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	intercept := verifier.StreamInterceptor(v, slog.Default())
	ierr := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error { return nil })

	require.Error(t, ierr)
	assert.Equal(t, codes.Unauthenticated, status.Code(ierr))

	msg := status.Convert(ierr).Message()
	assert.Contains(t, strings.ToLower(msg), "unknown kid",
		"an unknown-kid rejection must name its cause; got %q", msg)
	assert.NotContains(t, strings.ToLower(msg), "expired",
		"the token is INSIDE its validity window -- calling it expired sends the "+
			"operator to memql#3385 instead of the signing-key skew; got %q", msg)
}

// TestStreamInterceptor_GenuinelyExpiredToken_KeepsTheGenericMessage pins the
// other side of the split: a token that really has expired must NOT be
// reported as a signing-key problem. Without this, "make the unknown-kid case
// specific" could be satisfied by making every message mention kid.
func TestStreamInterceptor_GenuinelyExpiredToken_KeepsTheGenericMessage(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// Minted far enough in the past that it is expired even against the
	// verifier's 30s leeway.
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:erin",
	}, time.Now().UTC().Add(-48*time.Hour))
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	intercept := verifier.StreamInterceptor(v, slog.Default())
	ierr := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error { return nil })

	require.Error(t, ierr)
	assert.Equal(t, codes.Unauthenticated, status.Code(ierr))

	msg := status.Convert(ierr).Message()
	assert.Contains(t, strings.ToLower(msg), "expired",
		"a genuinely expired token must still read as expired; got %q", msg)
	assert.NotContains(t, strings.ToLower(msg), "kid",
		"an expired token is not a signing-key problem; got %q", msg)
}
