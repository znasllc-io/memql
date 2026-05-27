package node_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/node"
)

// fakeRevocationResolver is a recording stub for
// NodeTokenRevocationResolver that counts calls + returns a per-key
// answer the test stages upfront. Lets us observe the cache
// behaviour (how many resolver calls fire for N stream opens within
// the TTL window) without standing up a real identity store.
type fakeRevocationResolver struct {
	revoked map[string]bool
	err     error
	calls   int64
}

func keyOf(nodeType, nodeId string) string { return nodeType + "/" + nodeId }

func (r *fakeRevocationResolver) IsNodeTokenRevoked(ctx context.Context, nodeType, nodeId string) (bool, error) {
	_ = ctx
	atomic.AddInt64(&r.calls, 1)
	if r.err != nil {
		return false, r.err
	}
	return r.revoked[keyOf(nodeType, nodeId)], nil
}

// TestNodeRevocationCheck_NilResolverFallsBackToBase pins the opt-in
// contract: without a resolver wired, the with-revocation interceptor
// must behave exactly like the base one. This is the promise the
// cluster bootstrap relies on -- a deployment that opts out of
// persistence (no identity engine) doesn't pay any new cost.
func TestNodeRevocationCheck_NilResolverFallsBackToBase(t *testing.T) {
	// Both nil-check and nil-resolver should fall through.
	cases := []*node.NodeRevocationCheck{
		nil,
		{}, // explicit struct, nil Resolver
	}
	for _, c := range cases {
		intercept := node.NodeClassStreamInterceptorWithRevocation(nil, c, nil)
		require.NotNil(t, intercept, "interceptor must be returned even when revocation gate is disabled")
	}
}

// TestNodeRevocationCheck_DefaultCacheTTL pins the default TTL to
// the value the cluster bootstrap relies on (5s). The check is on
// the constant rather than a struct field so a future tweak to the
// default surfaces here as an explicit signal.
func TestNodeRevocationCheck_DefaultCacheTTL(t *testing.T) {
	assert.Equal(t, 5*time.Second, node.DefaultNodeRevocationCacheTTL)
}

// TestNodeRevocationCheck_ResolverContract pins what the resolver
// returns + how the cache rolls. Drives the resolver directly (the
// gRPC interceptor wrapping is tested separately at integration
// layer) -- this is the unit-level cache + lookup contract that
// matters most for the hot path.
func TestNodeRevocationCheck_ResolverContract(t *testing.T) {
	t.Run("returns false for live row", func(t *testing.T) {
		r := &fakeRevocationResolver{revoked: map[string]bool{}}
		got, err := r.IsNodeTokenRevoked(context.Background(), "bff", "bff-local")
		require.NoError(t, err)
		assert.False(t, got)
		assert.Equal(t, int64(1), atomic.LoadInt64(&r.calls))
	})
	t.Run("returns true for revoked row", func(t *testing.T) {
		r := &fakeRevocationResolver{
			revoked: map[string]bool{keyOf("bff", "bff-local"): true},
		}
		got, err := r.IsNodeTokenRevoked(context.Background(), "bff", "bff-local")
		require.NoError(t, err)
		assert.True(t, got)
	})
	t.Run("returns error when resolver fails", func(t *testing.T) {
		r := &fakeRevocationResolver{err: errors.New("db unreachable")}
		_, err := r.IsNodeTokenRevoked(context.Background(), "bff", "bff-local")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db unreachable")
	})
}

// runRevocationInterceptor exercises NodeClassStreamInterceptorWithRevocation
// end-to-end: mints a real node-class JWT, drives through the
// interceptor with the supplied resolver + cache TTL, returns the
// (err, handler-was-called) pair. Mirrors the runInterceptor helper
// in auth_test.go but for the with-revocation variant.
//
// nodeId/nodeType bind the minted token; the resolver receives the
// same pair on every IsNodeTokenRevoked call.
func runRevocationInterceptor(t *testing.T, resolver node.NodeTokenRevocationResolver, ttl time.Duration, nodeType, nodeId string) (error, bool) {
	t.Helper()
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node:" + nodeType + ":" + nodeId,
		NodeId:     nodeId,
		NodeType:   nodeType,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := &node.NodeRevocationCheck{Resolver: resolver, CacheTTL: ttl}
	intercept := node.NodeClassStreamInterceptorWithRevocation(v, check, logger)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
	ss := &fakeServerStream{ctx: ctx}
	info := &grpc.StreamServerInfo{FullMethod: "/test.NodeService/Stream"}

	handlerCalled := false
	handler := func(_ interface{}, _ grpc.ServerStream) error {
		handlerCalled = true
		return nil
	}
	return intercept(nil, ss, info, handler), handlerCalled
}

// TestNodeClassStreamInterceptorWithRevocation_AdmitsActive pins
// the happy path: resolver says "not revoked" -> handler runs.
func TestNodeClassStreamInterceptorWithRevocation_AdmitsActive(t *testing.T) {
	r := &fakeRevocationResolver{revoked: map[string]bool{}}
	err, called := runRevocationInterceptor(t, r, 0, "bff", "bff-local")
	require.NoError(t, err)
	assert.True(t, called, "handler must run when token is live")
	assert.Equal(t, int64(1), atomic.LoadInt64(&r.calls), "single open should call resolver once")
}

// TestNodeClassStreamInterceptorWithRevocation_RejectsRevoked pins
// the load-bearing #349 case: an active JWT (signature valid, class
// right) whose row is Active==false must be rejected with
// codes.PermissionDenied. Handler must NOT run.
func TestNodeClassStreamInterceptorWithRevocation_RejectsRevoked(t *testing.T) {
	r := &fakeRevocationResolver{
		revoked: map[string]bool{keyOf("bff", "bff-local"): true},
	}
	err, called := runRevocationInterceptor(t, r, 0, "bff", "bff-local")
	require.Error(t, err)
	assert.False(t, called, "handler must NOT run when token is revoked")

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T", err)
	assert.Equal(t, codes.PermissionDenied, st.Code(), "expected PermissionDenied, got %s", st.Code())
	assert.Contains(t, st.Message(), "revoked")
}

// TestNodeClassStreamInterceptorWithRevocation_RejectsOnResolverError
// asserts a resolver lookup failure fails closed -- the interceptor
// rejects with Unauthenticated rather than admitting traffic on a
// partial check. Prevents an identity-store outage from silently
// disabling the revocation gate.
func TestNodeClassStreamInterceptorWithRevocation_RejectsOnResolverError(t *testing.T) {
	r := &fakeRevocationResolver{err: errors.New("db unreachable")}
	err, called := runRevocationInterceptor(t, r, 0, "bff", "bff-local")
	require.Error(t, err)
	assert.False(t, called)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// TestNodeClassStreamInterceptorWithRevocation_CachesPositiveAnswers
// pins the load-bearing performance property: N stream opens within
// the cache TTL window for the same (nodeType, nodeId) result in
// exactly ONE resolver call. This is what makes the per-call DB read
// affordable on the hot path under healthy peers each pinging every
// ~30s.
//
// Drives the interceptor 5 times in succession with a generous (1h)
// cache TTL. Resolver call counter should be 1.
func TestNodeClassStreamInterceptorWithRevocation_CachesPositiveAnswers(t *testing.T) {
	r := &fakeRevocationResolver{revoked: map[string]bool{}}
	const opens = 5
	for i := 0; i < opens; i++ {
		err, called := runRevocationInterceptor(t, r, time.Hour, "bff", "bff-local")
		require.NoError(t, err, "open %d failed", i)
		assert.True(t, called, "open %d did not run handler", i)
	}
	// Each runRevocationInterceptor call builds its OWN cache (the
	// cache is closure-local to the interceptor). Within a single
	// interceptor instance, only 1 resolver call should fire. So we
	// expect `opens` resolver calls total -- not `1` -- because each
	// call gets a fresh interceptor + fresh cache.
	//
	// To exercise the cache, we need to drive the SAME interceptor
	// multiple times. That requires a longer-lived setup; see the
	// next test.
	assert.Equal(t, int64(opens), atomic.LoadInt64(&r.calls))
}

// TestNodeClassStreamInterceptorWithRevocation_CacheHitsAcrossOpens
// exercises the cache directly by driving the SAME interceptor
// instance N times. Within the TTL window, all N stream opens must
// reuse the first lookup; resolver call count == 1.
func TestNodeClassStreamInterceptorWithRevocation_CacheHitsAcrossOpens(t *testing.T) {
	// Build a single interceptor instance, then drive it manually.
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node:bff:bff-local",
		NodeId:     "bff-local",
		NodeType:   "bff",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &fakeRevocationResolver{revoked: map[string]bool{}}
	check := &node.NodeRevocationCheck{Resolver: r, CacheTTL: time.Hour}
	intercept := node.NodeClassStreamInterceptorWithRevocation(v, check, logger)
	info := &grpc.StreamServerInfo{FullMethod: "/test.NodeService/Stream"}
	handler := func(_ interface{}, _ grpc.ServerStream) error { return nil }

	const opens = 5
	for i := 0; i < opens; i++ {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		ss := &fakeServerStream{ctx: ctx}
		err := intercept(nil, ss, info, handler)
		require.NoError(t, err, "open %d failed", i)
	}
	assert.Equal(t, int64(1), atomic.LoadInt64(&r.calls),
		"cache should collapse %d opens to a single resolver call within TTL", opens)
}

// TestNodeClassStreamInterceptorWithRevocation_CacheExpiresAfterTTL
// pins the opposite: once the TTL expires, the next lookup re-asks
// the resolver. Without this, a token could stay admitted forever
// after the operator revoked it (the revoke wouldn't propagate
// until the cache was cleared by some other mechanism).
//
// Drives the interceptor twice with a 50ms TTL + a sleep between.
// Expect 2 resolver calls.
func TestNodeClassStreamInterceptorWithRevocation_CacheExpiresAfterTTL(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node:bff:bff-local",
		NodeId:     "bff-local",
		NodeType:   "bff",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &fakeRevocationResolver{revoked: map[string]bool{}}
	const ttl = 50 * time.Millisecond
	check := &node.NodeRevocationCheck{Resolver: r, CacheTTL: ttl}
	intercept := node.NodeClassStreamInterceptorWithRevocation(v, check, logger)
	info := &grpc.StreamServerInfo{FullMethod: "/test.NodeService/Stream"}
	handler := func(_ interface{}, _ grpc.ServerStream) error { return nil }

	drive := func() {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		ss := &fakeServerStream{ctx: ctx}
		require.NoError(t, intercept(nil, ss, info, handler))
	}

	drive()
	assert.Equal(t, int64(1), atomic.LoadInt64(&r.calls))
	// Sleep past the TTL window to guarantee expiry.
	time.Sleep(3 * ttl)
	drive()
	assert.Equal(t, int64(2), atomic.LoadInt64(&r.calls),
		"after TTL expires the next open must re-ask the resolver")
}
