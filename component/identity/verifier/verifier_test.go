package verifier_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestRevocationEpochClaimRoundTrip verifies the issuer stamps the
// epoch into the JWT and the verifier reads it back into
// VerifiedClaims. The full revocation flow is covered downstream;
// this test pins just the claim plumbing.
func TestRevocationEpochClaimRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:frank",
		RevocationEpoch: 7,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL)
	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, int64(7), vc.RevocationEpoch)
}

// TestVerifyRejectsStaleEpoch is the load-bearing #106 regression.
// Mint a token at epoch 0, then wire an EpochResolver that reports
// the user's current epoch as 1, and confirm the verifier rejects.
func TestVerifyRejectsStaleEpoch(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:grace",
		RevocationEpoch: 0,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL).WithEpochResolver(
		func(_ context.Context, userId string) (int64, error) {
			if userId == "v1:identity:user:grace" {
				return 1, nil
			}
			return 0, nil
		},
	)

	_, err = v.VerifyBearer(context.Background(), tok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoked")
}

// TestVerifyAdmitsSameEpoch confirms the rejection rule is
// strict-greater: token epoch == user epoch admits cleanly. Otherwise
// every never-revoked user (with epoch 0) would fail validation as
// soon as the resolver was wired.
func TestVerifyAdmitsSameEpoch(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:hank",
		RevocationEpoch: 5,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL).WithEpochResolver(
		func(_ context.Context, _ string) (int64, error) { return 5, nil },
	)

	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, int64(5), vc.RevocationEpoch)
}

// TestVerifyFailOpenOnResolverError pins the fail-open behavior --
// transient resolver errors don't disconnect authenticated streams.
// A DB hiccup in the identity service shouldn't take down every
// other node's auth gate.
func TestVerifyFailOpenOnResolverError(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:          "v1:identity:user:irene",
		RevocationEpoch: 0,
	}, time.Now().UTC())
	require.NoError(t, err)

	v := newVerifier(t, srv.URL).WithEpochResolver(
		func(_ context.Context, _ string) (int64, error) {
			return 0, assertErr("identity unreachable")
		},
	)

	vc, err := v.VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, "v1:identity:user:irene", vc.UserId)
}

// TestBindRevocationWatcherCancelsOnBump verifies the periodic
// in-stream check: a stream that survives long enough sees its
// context canceled when the user's epoch advances.
func TestBindRevocationWatcherCancelsOnBump(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, _ := newIdentityIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	// Tight interval so the test doesn't sit on a 5-min timer.
	cfg := verifier.Config{
		BaseURL:                 srv.URL,
		ExpectedAudience:        "memql",
		JWKSRefreshInterval:     time.Hour,
		JWKSFetchTimeout:        5 * time.Second,
		RevocationCheckInterval: 25 * time.Millisecond,
	}
	cache, err := verifier.NewJWKSCache(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	v, err := verifier.New(cfg, cache, nil, logger)
	require.NoError(t, err)

	// Resolver returns 0 initially, then 1 after a flag flips.
	var current int64 = 0
	v.WithEpochResolver(func(_ context.Context, _ string) (int64, error) {
		// Read without sync -- the test orchestrates the flip via
		// channel signaling below; race is benign for the test.
		return current, nil
	})

	vc := &verifier.VerifiedClaims{
		UserId:          "v1:identity:user:jess",
		RevocationEpoch: 0,
		Source:          verifier.SourceJWT,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	watched := v.BindRevocationWatcher(ctx, vc)

	// Bump the epoch after a moment; the watcher should pick it up
	// on its next tick (interval=25ms) and cancel `watched`.
	go func() {
		time.Sleep(40 * time.Millisecond)
		current = 1
	}()

	select {
	case <-watched.Done():
		// Expected -- watcher canceled the stream.
	case <-time.After(1 * time.Second):
		t.Fatalf("watcher did not cancel stream after epoch bump")
	}
	require.Error(t, watched.Err())
	_ = km // referenced via mux JWKS handler
}

// assertErr is a tiny helper so the fail-open test doesn't pull in
// errors.New just for one call site.
type assertErr string

func (e assertErr) Error() string { return string(e) }
