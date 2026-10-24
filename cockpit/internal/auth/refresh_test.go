package auth

// Tests for the silent refresh-token grant path. The contract being
// verified here is the one documented in cli/auth/token.go:
// EnsureValidToken: a cached refresh token must be exchanged
// against `/auth/refresh` BEFORE we ever fall through to the browser
// flow. Anything that regresses that contract pops a browser tab
// in the user's face every 3-15 minutes, which is the failure mode
// that drove this whole change.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// TestRefresh_Success exercises the happy path: identity rotates the
// refresh token, returns a fresh pair, Refresh hands them back in a
// LoginResult.
func TestRefresh_Success(t *testing.T) {
	var sawBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&sawBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"token_type":    "Bearer",
			"expires_in":    180,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer ts.Close()

	res, err := Refresh(context.Background(), ts.URL, "old-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if sawBody["refresh_token"] != "old-refresh" {
		t.Errorf("server saw refresh_token=%q, want old-refresh", sawBody["refresh_token"])
	}
	if res.AccessToken != "new-access" {
		t.Errorf("AccessToken = %q, want new-access", res.AccessToken)
	}
	if res.RefreshToken != "rotated-refresh" {
		t.Errorf("RefreshToken = %q, want rotated-refresh", res.RefreshToken)
	}
	if remaining := time.Until(res.Expiry); remaining < 2*time.Minute || remaining > 4*time.Minute {
		t.Errorf("Expiry %v is not ~3 minutes from now", res.Expiry)
	}
}

// TestRefresh_InvalidGrant covers the terminal failure mode: identity
// returns 401 with error=invalid_grant. Refresh must surface
// ErrInvalidGrant so the caller can distinguish "session is dead" from
// "transient blip" and open the browser only in the former case.
func TestRefresh_InvalidGrant(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "refresh token is no longer valid",
		})
	}))
	defer ts.Close()

	_, err := Refresh(context.Background(), ts.URL, "stale-refresh")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expected errors.Is(err, ErrInvalidGrant); err = %v", err)
	}
	if !strings.Contains(err.Error(), "refresh token is no longer valid") {
		t.Errorf("error should surface the server's error_description; got %q", err)
	}
}

// TestRefresh_TransientServerError covers the case where identity is
// down or having a bad time (5xx). Refresh must NOT return
// ErrInvalidGrant in this case -- the session may still be valid and
// the caller should retry rather than pop a browser tab.
func TestRefresh_TransientServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error","error_description":"db down"}`))
	}))
	defer ts.Close()

	_, err := Refresh(context.Background(), ts.URL, "any-refresh")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("5xx must not be classified as invalid_grant; err = %v", err)
	}
}

// TestRefresh_RotatedTokenOmitted covers identity's cookie-only
// rotation mode: the JSON response carries no refresh_token (the new
// value rode the Set-Cookie header instead). Refresh must reuse the
// caller's existing refresh token so we still have something to
// present on the next call.
func TestRefresh_RotatedTokenOmitted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access",
			"token_type":   "Bearer",
			"expires_in":   180,
		})
	}))
	defer ts.Close()

	res, err := Refresh(context.Background(), ts.URL, "preserved-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.RefreshToken != "preserved-refresh" {
		t.Errorf("RefreshToken = %q, want preserved-refresh (server omitted it; should reuse caller's)", res.RefreshToken)
	}
}

// TestRefresh_EmptyToken guards the precondition that
// EnsureValidToken relies on -- Refresh refuses to fire without an
// actual token, which avoids accidentally hitting identity with an
// empty-body request that would 401 and look like ErrInvalidGrant.
func TestRefresh_EmptyToken(t *testing.T) {
	_, err := Refresh(context.Background(), "http://example.invalid", "")
	if err == nil {
		t.Fatal("expected error for empty refresh token")
	}
	if errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("empty-input error must not be ErrInvalidGrant; got %v", err)
	}
}

// TestEnsureValidToken_UsesRefreshBeforeLogin is the integration-level
// assertion: with a cached token whose refresh is still valid,
// EnsureValidToken must hit /auth/refresh and return the new access
// token WITHOUT touching the browser flow. This is the regression
// guard for the pre-refactor behavior (browser tab every ~15min).
func TestEnsureValidToken_UsesRefreshBeforeLogin(t *testing.T) {
	withTempHome(t)

	var refreshCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-access",
				"token_type":    "Bearer",
				"expires_in":    180,
				"refresh_token": "rotated-refresh",
			})
		case "/login":
			// The whole point of the new branch is that we DON'T land
			// here. A request to /login means the refresh path was
			// bypassed; fail the test loudly.
			t.Errorf("EnsureValidToken hit /login when refresh should have been used")
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	// Seed the cache with an expired access token + valid refresh token.
	// IsExpired's 60s buffer means anything within the next minute counts
	// as "expired" -- we backdate Expiry to force the branch.
	if err := config.SaveToken("test-cluster", &config.StoredToken{
		AccessToken:  "stale-access",
		RefreshToken: "valid-refresh",
		Expiry:       time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	token, err := EnsureValidToken(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Issuer:   ts.URL,
		ClientId: "cockpit",
	})
	if err != nil {
		t.Fatalf("EnsureValidToken: %v", err)
	}
	if token != "fresh-access" {
		t.Fatalf("returned token = %q, want fresh-access (the refreshed one)", token)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("expected exactly one /auth/refresh call, got %d", got)
	}

	// Verify the rolled token landed on disk so the NEXT call can hit
	// the cached-fresh branch without round-tripping identity.
	stored, err := config.LoadToken("test-cluster")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if stored == nil || stored.AccessToken != "fresh-access" || stored.RefreshToken != "rotated-refresh" {
		t.Errorf("on-disk token not updated: %+v", stored)
	}
}

// TestEnsureValidToken_TransientRefreshErrorReturnsError covers the
// "don't pop a browser on a network blip" rule: a 5xx from
// /auth/refresh must surface as an error (so the dial fails and the
// pool's backoff kicks in), NOT silently fall through to Login().
func TestEnsureValidToken_TransientRefreshErrorReturnsError(t *testing.T) {
	withTempHome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal_error"}`))
		case "/login":
			t.Errorf("EnsureValidToken hit /login on a transient refresh failure; should have returned an error instead")
		}
	}))
	defer ts.Close()

	if err := config.SaveToken("test-cluster", &config.StoredToken{
		AccessToken:  "stale",
		RefreshToken: "valid",
		Expiry:       time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	_, err := EnsureValidToken(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Issuer:   ts.URL,
		ClientId: "cockpit",
	})
	if err == nil {
		t.Fatal("expected error from transient refresh failure; got nil")
	}
	if !strings.Contains(err.Error(), "refresh access token") {
		t.Errorf("expected wrapped 'refresh access token' error; got %q", err)
	}
}

// TestEnsureValidToken_CachedFreshShortCircuits is the negative-space
// check on the cache-hit branch: a token that is well within its
// expiry window should be returned without contacting identity AT
// ALL. If the server records any HTTP hit, something in the refresh
// path is firing eagerly.
func TestEnsureValidToken_CachedFreshShortCircuits(t *testing.T) {
	withTempHome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("identity got an unexpected hit on %s; cached-fresh path must not call out", r.URL.Path)
	}))
	defer ts.Close()

	if err := config.SaveToken("test-cluster", &config.StoredToken{
		AccessToken:  "fresh-access",
		RefreshToken: "valid",
		Expiry:       time.Now().Add(10 * time.Minute), // well outside the 60s buffer
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	token, err := EnsureValidToken(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Issuer:   ts.URL,
		ClientId: "cockpit",
	})
	if err != nil {
		t.Fatalf("EnsureValidToken: %v", err)
	}
	if token != "fresh-access" {
		t.Errorf("returned token = %q, want fresh-access (the cached one)", token)
	}
}

// withTempHome points HOME at a t.TempDir so config.SaveToken /
// LoadToken don't trample the developer's real ~/.memql while the
// test runs. Restored on test cleanup.
func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev, hadPrev := os.LookupEnv("HOME")
	t.Setenv("HOME", dir)
	// Sanity check: config.ConfigDir() should now resolve under our
	// tempdir; if not, the test would silently write to the real
	// home directory and tests would race each other.
	if !strings.HasPrefix(config.ConfigDir(), filepath.Clean(dir)) {
		t.Fatalf("ConfigDir() = %q does not point inside %q; aborting before we touch the real home directory", config.ConfigDir(), dir)
	}
	if hadPrev {
		t.Cleanup(func() { _ = os.Setenv("HOME", prev) })
	}
}
