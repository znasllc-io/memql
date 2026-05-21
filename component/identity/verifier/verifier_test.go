package verifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// jwksHandler returns an http.Handler that serves the live JWKS from
// the supplied KeyManager. Mirrors identity.JWKSHandler so the test
// doesn't need to depend on the embedded one.
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

// newIdentityIssuer spins up a fresh KeyManager + JWTIssuer rooted in
// a temp dir so each test gets isolated key material.
func newIdentityIssuer(t *testing.T, baseURL string) (*identity.KeyManager, *identity.JWTIssuer) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     baseURL,
		JWTAudience: "memql",
		KeyDir:      dir,
	}
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
	cache, err := verifier.NewJWKSCache(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	v, err := verifier.New(cfg, cache, nil, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	return v
}

func TestVerifyJWTHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	now := time.Now().UTC()
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:alice",
		Email:  "alice@example.com",
		Name:   "Alice",
		Role:   "writer",
	}, now)
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, "v1:identity:user:alice", vc.UserId)
	assert.Equal(t, "alice@example.com", vc.Email)
	assert.Equal(t, "writer", vc.Role)
	assert.Equal(t, verifier.SourceJWT, vc.Source)
}

func TestVerifyJWTSignatureMismatch(t *testing.T) {
	// Mint a token with one key, serve JWKS with a different key.
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	_, issA := newIdentityIssuer(t, srv.URL)
	kmB, _ := newIdentityIssuer(t, srv.URL)
	// JWKS serves keys from issuer B, but token was minted by issuer A.
	mux.Handle("/.well-known/jwks.json", jwksHandler(kmB))

	now := time.Now().UTC()
	tok, _, err := issA.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:bob",
	}, now)
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	_, err = v.VerifyBearer(context.Background(), tok)
	require.Error(t, err)
}

func TestVerifyJWTWrongAudience(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	// Issuer mints tokens with audience "other".
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     srv.URL,
		JWTAudience: "other",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: "v1:identity:user:carol"}, time.Now().UTC())
	require.NoError(t, err)

	// Verifier expects audience "memql".
	v := newVerifier(t, srv.URL)
	_, err = v.VerifyBearer(context.Background(), tok)
	require.Error(t, err)
}

func TestVerifyJWTExpired(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// Mint a token whose `iat`/`exp` are firmly in the past.
	past := time.Now().UTC().Add(-2 * time.Hour)
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: "v1:identity:user:dave"}, past)
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	_, err = v.VerifyBearer(context.Background(), tok)
	require.Error(t, err)
}

func TestVerifyJWTUnknownKidTriggersForceRefresh(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{Enabled: true, BaseURL: srv.URL, JWTAudience: "memql", KeyDir: dir}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// Build the verifier with an empty cache by pointing at JWKS BEFORE
	// any keys are published... actually, NewJWKSCache fetches once at
	// construction so we already have the original key. To exercise
	// force-refresh we rotate identity, mint a token under the new key,
	// and verify; the cache will see an unknown kid and refresh.
	v := newVerifier(t, srv.URL)
	_, err = km.Rotate(time.Hour)
	require.NoError(t, err)

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: "v1:identity:user:eve"}, time.Now().UTC())
	require.NoError(t, err)

	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, "v1:identity:user:eve", vc.UserId)
}

// ---------------------------------------------------------------------------
// memql#106 -- revocation_epoch claim round-trip + interceptor check
// ---------------------------------------------------------------------------

func TestVerifyJWT_RevocationEpoch_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:frank",
		RevocationEpoch: 42,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.EqualValues(t, 42, vc.RevocationEpoch)
}

func TestVerifyJWT_RevocationEpoch_AbsentDefaultsToZero(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// IssueInput leaves RevocationEpoch as the zero value.
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: "v1:identity:user:grace"}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.EqualValues(t, 0, vc.RevocationEpoch)
}

// ---------- interceptor harness ----------

// fakeStream is a minimal grpc.ServerStream that carries a ctx the
// test can observe. Mirrors the shape the real grpc transport hands
// to interceptors.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) SendMsg(any) error        { return nil }
func (f *fakeStream) RecvMsg(any) error        { return nil }

func ctxWithToken(tok string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
}

// epochResolverFunc lets a test plug a closure into the
// EpochResolver hole. The interceptor sees it as the interface.
type epochResolverFunc func(ctx context.Context, userId string) (int64, error)

func (f epochResolverFunc) CurrentEpoch(ctx context.Context, userId string) (int64, error) {
	return f(ctx, userId)
}

func mintEpochToken(t *testing.T, srv *httptest.Server, mux *http.ServeMux, epoch int64) (string, *identity.JWTIssuer) {
	t.Helper()
	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:harry",
		RevocationEpoch: epoch,
	}, time.Now().UTC())
	require.NoError(t, err)
	return tok, iss
}

func TestStreamInterceptor_EpochCheck_RejectsAtOpen(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	tok, _ := mintEpochToken(t, srv, mux, 3)
	v := newVerifier(t, srv.URL)

	resolver := epochResolverFunc(func(ctx context.Context, userId string) (int64, error) {
		return 5, nil // user's epoch advanced past the token claim
	})

	intercept := verifier.StreamInterceptorWithEpoch(v, slog.Default(), &verifier.EpochCheck{Resolver: resolver})

	handlerCalled := false
	err := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error {
			handlerCalled = true
			return nil
		})

	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.False(t, handlerCalled, "handler must NOT run when epoch check rejects open")
}

func TestStreamInterceptor_EpochCheck_AdmitsWhenEqualOrAhead(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	tok, _ := mintEpochToken(t, srv, mux, 5)
	v := newVerifier(t, srv.URL)

	for _, current := range []int64{0, 1, 5} {
		t.Run("current="+timeToString(current), func(t *testing.T) {
			resolver := epochResolverFunc(func(ctx context.Context, userId string) (int64, error) {
				return current, nil
			})
			intercept := verifier.StreamInterceptorWithEpoch(v, slog.Default(),
				&verifier.EpochCheck{Resolver: resolver, Interval: time.Hour})

			handlerCalled := false
			err := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
				&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
				func(srv any, stream grpc.ServerStream) error {
					handlerCalled = true
					return nil
				})
			require.NoError(t, err)
			assert.True(t, handlerCalled, "handler must run when token epoch >= current")
		})
	}
}

// timeToString is a tiny helper that lets us format int64 epoch
// values without importing strconv just for the subtest name. Not
// actually a time formatter despite the name -- shape parity with
// the surrounding test code.
func timeToString(n int64) string {
	if n < 0 {
		return "-" + timeToString(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return timeToString(n/10) + timeToString(n%10)
}

func TestStreamInterceptor_EpochCheck_PeriodicReCheckClosesStream(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	tok, _ := mintEpochToken(t, srv, mux, 5)
	v := newVerifier(t, srv.URL)

	var current atomic.Int64
	current.Store(5) // initial -- token epoch matches, stream opens

	resolver := epochResolverFunc(func(ctx context.Context, userId string) (int64, error) {
		return current.Load(), nil
	})

	intercept := verifier.StreamInterceptorWithEpoch(v, slog.Default(),
		&verifier.EpochCheck{Resolver: resolver, Interval: 50 * time.Millisecond})

	handlerObservedCancel := make(chan struct{}, 1)
	streamCtx := ctxWithToken(tok)
	err := intercept(nil, &fakeStream{ctx: streamCtx},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/LongLived"},
		func(srv any, stream grpc.ServerStream) error {
			// Simulate a long-lived handler that waits on
			// ctx.Done(). When the epoch advances, the re-check
			// goroutine should cancel the derived ctx.
			handlerCtx := stream.Context()
			// Wait briefly, then bump the epoch.
			time.AfterFunc(75*time.Millisecond, func() {
				current.Store(6)
			})
			select {
			case <-handlerCtx.Done():
				handlerObservedCancel <- struct{}{}
				return handlerCtx.Err()
			case <-time.After(3 * time.Second):
				return errors.New("timeout: handler ctx never cancelled")
			}
		})
	require.Error(t, err) // ctx.Err() comes back as context.Canceled
	select {
	case <-handlerObservedCancel:
		// ok
	default:
		t.Fatal("handler never observed ctx cancellation")
	}
}

func TestStreamInterceptor_EpochCheck_DisabledWhenResolverNil(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	// Token's epoch is 1; if the resolver were consulted it'd reject
	// because we'd pretend current is 999. With Resolver == nil the
	// interceptor must skip the check.
	tok, _ := mintEpochToken(t, srv, mux, 1)
	v := newVerifier(t, srv.URL)

	intercept := verifier.StreamInterceptor(v, slog.Default()) // no EpochCheck

	handlerCalled := false
	err := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error {
			handlerCalled = true
			return nil
		})
	require.NoError(t, err)
	assert.True(t, handlerCalled, "handler must run when EpochCheck is not wired")
}

func TestStreamInterceptor_EpochCheck_ResolverErrorRejects(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	tok, _ := mintEpochToken(t, srv, mux, 5)
	v := newVerifier(t, srv.URL)

	var calls atomic.Int32
	resolver := epochResolverFunc(func(ctx context.Context, userId string) (int64, error) {
		calls.Add(1)
		return 0, errors.New("db down")
	})

	intercept := verifier.StreamInterceptorWithEpoch(v, slog.Default(),
		&verifier.EpochCheck{Resolver: resolver})

	err := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error {
			return nil
		})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.EqualValues(t, 1, calls.Load(), "resolver must be consulted exactly once at open")
}

// TestStreamInterceptor_EpochCheck_PeriodicCleanupOnHandlerExit
// pins that the per-stream goroutine exits when the handler
// returns. We watch the resolver's call count -- once the handler
// returns, no further ticks should fire.
func TestStreamInterceptor_EpochCheck_PeriodicCleanupOnHandlerExit(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	tok, _ := mintEpochToken(t, srv, mux, 5)
	v := newVerifier(t, srv.URL)

	var (
		mu    sync.Mutex
		calls int
	)
	resolver := epochResolverFunc(func(ctx context.Context, userId string) (int64, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return 5, nil
	})

	intercept := verifier.StreamInterceptorWithEpoch(v, slog.Default(),
		&verifier.EpochCheck{Resolver: resolver, Interval: 20 * time.Millisecond})

	err := intercept(nil, &fakeStream{ctx: ctxWithToken(tok)},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Method"},
		func(srv any, stream grpc.ServerStream) error {
			// Handler returns quickly -- the goroutine should
			// observe ctx.Done() (via deferred cancel) and exit.
			time.Sleep(80 * time.Millisecond)
			return nil
		})
	require.NoError(t, err)

	// Snapshot the call count just after handler return.
	mu.Lock()
	afterReturn := calls
	mu.Unlock()

	// Wait long enough that several more ticks WOULD fire if the
	// goroutine were still alive.
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, afterReturn, calls, "no further resolver calls after handler returns -- goroutine must have stopped")
}

func TestVerifyEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, _ := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))
	v := newVerifier(t, srv.URL)
	_, err := v.VerifyBearer(context.Background(), "")
	require.Error(t, err)
}
