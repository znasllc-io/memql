package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestDeviceLoginHappyPath drives the whole RFC 8628 loop against a
// fake identity service: issuance, two authorization_pending polls
// (one raising the interval via slow_down), then tokens.
func TestDeviceLoginHappyPath(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["client_id"] != "cockpit" {
			t.Errorf("device/code body = %v, err = %v", body, err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-123",
			"user_code":                 "WDJB-MJHT",
			"verification_uri":          "https://identity.example/device",
			"verification_uri_complete": "https://identity.example/device?user_code=WDJB-MJHT",
			"expires_in":                600,
			"interval":                  0, // client must floor this to 5s; the test overrides polling via short deadline anyway
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != deviceGrantType || body["device_code"] != "dev-123" {
			t.Errorf("token body = %v", body)
		}
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "slow_down", "interval": 1})
		default:
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-xyz",
				"refresh_token": "rt-xyz",
				"expires_in":    3600,
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := requestDeviceCode(context.Background(), srv.URL, "cockpit")
	if err != nil {
		t.Fatalf("requestDeviceCode: %v", err)
	}
	if dc.UserCode != "WDJB-MJHT" || dc.DeviceCode != "dev-123" {
		t.Fatalf("unexpected issuance: %+v", dc)
	}

	// Poll directly (DeviceLogin's loop sleeps >= 5s per RFC; the unit
	// test exercises the transition table without the wall-clock).
	r1, retry1, _, err1 := pollDeviceToken(context.Background(), srv.URL, "cockpit", dc.DeviceCode)
	if r1 != nil || !retry1 || err1 != nil {
		t.Fatalf("poll 1: want pending, got result=%v retry=%v err=%v", r1, retry1, err1)
	}
	r2, retry2, raised, err2 := pollDeviceToken(context.Background(), srv.URL, "cockpit", dc.DeviceCode)
	if r2 != nil || !retry2 || err2 != nil || raised != 1*time.Second {
		t.Fatalf("poll 2: want slow_down interval=1s, got result=%v retry=%v raised=%v err=%v", r2, retry2, raised, err2)
	}
	r3, retry3, _, err3 := pollDeviceToken(context.Background(), srv.URL, "cockpit", dc.DeviceCode)
	if err3 != nil || retry3 || r3 == nil || r3.AccessToken != "at-xyz" || r3.RefreshToken != "rt-xyz" {
		t.Fatalf("poll 3: want tokens, got result=%+v retry=%v err=%v", r3, retry3, err3)
	}
	if time.Until(r3.Expiry) < 55*time.Minute {
		t.Fatalf("expiry not derived from expires_in: %v", r3.Expiry)
	}
}

// TestDeviceLoginTerminalErrors pins the two terminal RFC 8628
// outcomes to errors (not retries): the human refusing, and the
// window closing.
func TestDeviceLoginTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"access_denied", "denied"},
		{"expired_token", "expired"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": tc.code})
		}))
		result, retry, _, err := pollDeviceToken(context.Background(), srv.URL, "cockpit", "dev-1")
		srv.Close()
		if result != nil || retry || err == nil {
			t.Fatalf("%s: want terminal error, got result=%v retry=%v err=%v", tc.code, result, retry, err)
		}
	}
}
