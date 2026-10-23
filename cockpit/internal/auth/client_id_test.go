package auth

// The cockpit does not store its own OAuth client in the shared
// ~/.memql/clusters.yaml (config.ClusterConfig.ClientId), so an entry
// with no client_id is the NORMAL case. These tests pin that such an
// entry still signs in and refreshes as "cockpit", and that an explicit
// client_id is still what the cockpit presents.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// stubInteractiveLogin replaces the browser / device sign-in for one
// test and records the client it was asked to present.
func stubInteractiveLogin(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	prev := interactiveLogin
	interactiveLogin = func(_ context.Context, _ string, clientId string) (*LoginResult, error) {
		asked = append(asked, clientId)
		return &LoginResult{
			AccessToken:  "signed-in-access",
			RefreshToken: "signed-in-refresh",
			Expiry:       time.Now().Add(10 * time.Minute),
		}, nil
	}
	t.Cleanup(func() { interactiveLogin = prev })
	return &asked
}

func TestEnsureValidToken_SignsInAsCockpitWhenClientIdAbsent(t *testing.T) {
	withTempHome(t)
	asked := stubInteractiveLogin(t)

	token, err := EnsureValidToken(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Endpoint: "https://api.example.com",
		Issuer:   "https://identity.example.com",
		// No ClientId: what `memql cluster add` now writes.
	})
	if err != nil {
		t.Fatalf("EnsureValidToken refused an entry with no client_id: %v", err)
	}
	if token != "signed-in-access" {
		t.Errorf("token = %q, want the sign-in's access token", token)
	}
	if len(*asked) != 1 || (*asked)[0] != "cockpit" {
		t.Fatalf("sign-in presented client %q, want exactly one sign-in as %q", *asked, "cockpit")
	}
}

func TestEnsureValidToken_SignsInAsExplicitClientId(t *testing.T) {
	withTempHome(t)
	asked := stubInteractiveLogin(t)

	if _, err := EnsureValidToken(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Endpoint: "https://api.example.com",
		Issuer:   "https://identity.example.com",
		ClientId: "operator-client",
	}); err != nil {
		t.Fatalf("EnsureValidToken: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "operator-client" {
		t.Fatalf("sign-in presented client %q, want the explicit operator-client", *asked)
	}
}

func TestEnsureValidToken_RefreshesWhenClientIdAbsent(t *testing.T) {
	withTempHome(t)
	asked := stubInteractiveLogin(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-access",
			"token_type":    "Bearer",
			"expires_in":    180,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer ts.Close()

	if err := config.SaveToken("test-cluster", &config.StoredToken{
		AccessToken:  "stale-access",
		RefreshToken: "valid-refresh",
		Expiry:       time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	token, err := EnsureValidTokenNonInteractive(context.Background(), config.ClusterConfig{
		Name:     "test-cluster",
		Endpoint: "https://api.example.com",
		Issuer:   ts.URL,
	}, nil)
	if err != nil {
		t.Fatalf("refresh refused an entry with no client_id: %v", err)
	}
	if token != "refreshed-access" {
		t.Errorf("token = %q, want the refreshed access token", token)
	}
	if len(*asked) != 0 {
		t.Errorf("a refreshable session opened a sign-in (%q)", *asked)
	}
}
