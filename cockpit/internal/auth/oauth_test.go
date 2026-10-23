package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRandomString(t *testing.T) {
	s1, err := randomString(32)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s2, err := randomString(32)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1 == "" || s2 == "" {
		t.Error("random strings should not be empty")
	}
	if s1 == s2 {
		t.Error("two random strings should differ")
	}
}

func TestExchangeCodeForToken_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "wrong content-type", http.StatusUnsupportedMediaType)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body["grant_type"] != "authorization_code" || body["code"] != "ABC" || body["client_id"] != "cockpit" || body["code_verifier"] != "test-verifier" {
			http.Error(w, "missing field", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-tok",
			"token_type":    "Bearer",
			"expires_in":    900,
			"refresh_token": "refresh-tok",
		})
	}))
	defer ts.Close()

	res, err := exchangeCodeForToken(context.Background(), ts.URL, "cockpit", "http://127.0.0.1:54321/cockpit/callback", "ABC", "test-verifier")
	if err != nil {
		t.Fatalf("exchangeCodeForToken: %v", err)
	}
	if res.AccessToken != "access-tok" {
		t.Errorf("AccessToken = %q, want access-tok", res.AccessToken)
	}
	if res.RefreshToken != "refresh-tok" {
		t.Errorf("RefreshToken = %q, want refresh-tok", res.RefreshToken)
	}
	if time.Until(res.Expiry) < 14*time.Minute {
		t.Errorf("Expiry = %v, expected ~15 minutes from now", res.Expiry)
	}
}

func TestExchangeCodeForToken_OAuthError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "auth code has expired",
		})
	}))
	defer ts.Close()

	_, err := exchangeCodeForToken(context.Background(), ts.URL, "cockpit", "http://127.0.0.1:54321/cockpit/callback", "ABC", "test-verifier")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "auth code has expired") {
		t.Errorf("error %q should surface the OAuth error fields", err)
	}
}

func TestExchangeCodeForToken_MissingAccessToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer",
			"expires_in": 900,
		})
	}))
	defer ts.Close()

	_, err := exchangeCodeForToken(context.Background(), ts.URL, "cockpit", "http://127.0.0.1:54321/cockpit/callback", "ABC", "test-verifier")
	if err == nil || !strings.Contains(err.Error(), "missing access_token") {
		t.Errorf("expected missing access_token error, got %v", err)
	}
}
