package node_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/node"
)

// jwksHandler exposes the issuer's JWKS for the verifier to fetch.
// Mirrors the helper in component/identity/verifier/verifier_test.go.
func jwksHandler(km *identity.KeyManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(identity.BuildJWKS(km, time.Now().UTC()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func newIssuer(t *testing.T, baseURL string) (*identity.KeyManager, *identity.JWTIssuer) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{Enabled: true, BaseURL: baseURL, JWTAudience: "memql", KeyDir: dir}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)
	return km, iss
}

func newVerifier(t *testing.T, baseURL string) *verifier.Verifier {
	t.Helper()
	cfg := verifier.Config{
		BaseURL:             baseURL,
		ExpectedAudience:    "memql",
		JWKSRefreshInterval: time.Hour,
		JWKSFetchTimeout:    5 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cache, err := verifier.NewJWKSCache(cfg, logger)
	require.NoError(t, err)
	v, err := verifier.New(cfg, cache, nil, logger)
	require.NoError(t, err)
	return v
}

// runInterceptor exercises NodeClassStreamInterceptor against a
// faked grpc.ServerStream carrying the supplied metadata. Returns
// the interceptor's error (nil = admitted), plus the handler's
// observed context (for binding inspection).
func runInterceptor(t *testing.T, v *verifier.Verifier, md metadata.MD) (error, context.Context) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	interceptor := node.NodeClassStreamInterceptor(v, logger)

	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &fakeServerStream{ctx: ctx}
	info := &grpc.StreamServerInfo{FullMethod: "/test.NodeService/Stream"}

	var observed context.Context
	handler := func(_ interface{}, hs grpc.ServerStream) error {
		observed = hs.Context()
		return nil
	}
	err := interceptor(nil, ss, info, handler)
	return err, observed
}

// TestNodeClassStreamInterceptor_AdmitsNodeClassToken pins the
// happy path: a class="node" JWT with bound node_id / node_type
// reaches the handler and the binding is on the handler's ctx.
func TestNodeClassStreamInterceptor_AdmitsNodeClassToken(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node-cog-1",
		NodeId:     "v1:cluster:node:cognition-1",
		NodeType:   "cognition",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	md := metadata.Pairs("authorization", "Bearer "+tok)
	err, observed := runInterceptor(t, v, md)
	require.NoError(t, err)
	require.NotNil(t, observed)

	gotId, gotType, ok := node.NodeBindingFromContext(observed)
	require.True(t, ok, "binding not on handler ctx")
	assert.Equal(t, "v1:cluster:node:cognition-1", gotId)
	assert.Equal(t, "cognition", gotType)
}

// TestNodeClassStreamInterceptor_RejectsUserClassToken is the
// load-bearing #105 case: a plain user-class JWT (the default mint
// shape) must NOT speak NodeService.Stream.
func TestNodeClassStreamInterceptor_RejectsUserClassToken(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:alice",
		Email:  "alice@example.com",
		Role:   "writer",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	md := metadata.Pairs("authorization", "Bearer "+tok)
	err, _ = runInterceptor(t, v, md)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected a gRPC status error")
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, strings.ToLower(st.Message()), "node-class")
}

// TestNodeClassStreamInterceptor_RejectsMissingToken: an inbound
// stream with no Authorization header gets Unauthenticated.
func TestNodeClassStreamInterceptor_RejectsMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux
	km, _ := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	v := newVerifier(t, srv.URL)
	err, _ := runInterceptor(t, v, metadata.MD{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// TestNodeClassStreamInterceptor_RejectsExpiredNodeToken pins that
// the standard JWT validation pipeline (exp / nbf / signature)
// still applies: a class="node" token past its expiry is rejected
// at the verifier step, before the class pin even fires.
func TestNodeClassStreamInterceptor_RejectsExpiredNodeToken(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// Mint a node token with iat 2h in the past + a 1h TTL.
	past := time.Now().UTC().Add(-2 * time.Hour)
	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId:  "v1:identity:identity:node-x",
		NodeId:      "v1:cluster:node:x",
		NodeType:    "bff",
		TTLOverride: time.Hour,
	}, past)
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	md := metadata.Pairs("authorization", "Bearer "+tok)
	err, _ = runInterceptor(t, v, md)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// TestNodeClassStreamInterceptor_NoopWhenVerifierNil confirms the
// backward-compat path: bootstrap that hasn't enabled
// MEMQL_NODE_REQUIRE_AUTH passes a nil verifier and every stream
// admits unchanged.
func TestNodeClassStreamInterceptor_NoopWhenVerifierNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	interceptor := node.NodeClassStreamInterceptor(nil, logger)

	ctx := context.Background()
	ss := &fakeServerStream{ctx: ctx}
	info := &grpc.StreamServerInfo{FullMethod: "/test.NodeService/Stream"}
	called := false
	err := interceptor(nil, ss, info, func(_ interface{}, _ grpc.ServerStream) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called, "handler not invoked on no-op path")
}

// fakeServerStream is a minimal grpc.ServerStream implementation
// sufficient for the interceptor's needs (Context only; no Send /
// Recv).
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }
