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

	"github.com/visionarys-io/memql/component/identity"
	"github.com/visionarys-io/memql/component/identity/verifier"
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
