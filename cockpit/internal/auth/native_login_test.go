package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestNativeLoginPKCEAndState(t *testing.T) {
	var challenge, redirect string
	exchanged := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		sum := sha256.Sum256([]byte(body["code_verifier"]))
		if body["client_id"] != "cockpit" || body["redirect_uri"] != redirect || body["code"] != "accepted-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			t.Error("exchange did not carry the bound native client, callback and PKCE verifier")
		}
		exchanged++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-access", "expires_in": 900})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := loginWithBrowser(ctx, server.URL, "cockpit", func(raw string) error {
		u, e := url.Parse(raw)
		if e != nil {
			return e
		}
		q := u.Query()
		if u.Path != "/authorize" || q.Get("client_id") != "cockpit" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 32 {
			t.Fatal("invalid native authorization request")
		}
		challenge = q.Get("code_challenge")
		redirect = q.Get("redirect_uri")
		for _, state := range []string{"", "forged"} {
			resp, e := http.Get(redirect + "?code=forged&state=" + state)
			if e != nil {
				return e
			}
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Error("callback accepted missing or forged state")
			}
		}
		resp, e := http.Get(redirect + "?code=accepted-code&state=" + url.QueryEscape(q.Get("state")))
		if e != nil {
			return e
		}
		resp.Body.Close()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "test-access" || exchanged != 1 {
		t.Fatalf("unexpected completion: exchanges=%d", exchanged)
	}
}
