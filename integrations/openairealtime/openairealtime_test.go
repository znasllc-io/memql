package openairealtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// decodeNode unwraps the single MemoryNode a capability returns into its
// JSON payload map.
func decodeNode(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

func newTestIntegration(t *testing.T, handler http.HandlerFunc) *Integration {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	i := New(func(_ context.Context, name string) (string, error) {
		if name == secretAPIKeyPrimary {
			return "sk-test-standing-key", nil
		}
		return "", http.ErrNoLocation // any error -> resolver tries the next name
	}, nil)
	i.apiBase = srv.URL
	return i
}

func TestCreateClientSecret_GAShape(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	i := newTestIntegration(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// GA shape: secret at top-level `value`.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value":      "ek_ga_abc123",
			"expires_at": float64(1735684800),
			"session":    map[string]any{"model": "gpt-realtime-2"},
		})
	})

	nodes, err := i.handleCreateClientSecret(context.Background(), map[string]any{}, 0)
	if err != nil {
		t.Fatalf("handleCreateClientSecret: %v", err)
	}
	out := decodeNode(t, nodes)

	if out["value"] != "ek_ga_abc123" {
		t.Errorf("value = %v, want ek_ga_abc123", out["value"])
	}
	if out["expiresAt"] != float64(1735684800) {
		t.Errorf("expiresAt = %v, want 1735684800", out["expiresAt"])
	}
	cfg, _ := out["config"].(map[string]any)
	if cfg["model"] != defaultModel {
		t.Errorf("config.model = %v, want %s", cfg["model"], defaultModel)
	}
	if cfg["voice"] != defaultVoice {
		t.Errorf("config.voice = %v, want %s", cfg["voice"], defaultVoice)
	}

	// Request shaping: Bearer auth from the resolved standing key, GA path,
	// and the model nested under `session`.
	if gotAuth != "Bearer sk-test-standing-key" {
		t.Errorf("Authorization = %q, want Bearer sk-test-standing-key", gotAuth)
	}
	if gotPath != clientSecretPath {
		t.Errorf("path = %q, want %q", gotPath, clientSecretPath)
	}
	session, _ := gotBody["session"].(map[string]any)
	if session["model"] != defaultModel {
		t.Errorf("request session.model = %v, want %s", session["model"], defaultModel)
	}
	if session["type"] != "realtime" {
		t.Errorf("request session.type = %v, want realtime", session["type"])
	}
}

func TestCreateClientSecret_LegacyNestedShape(t *testing.T) {
	// Pre-GA /realtime/sessions nested the secret under client_secret.value;
	// mint() must tolerate it.
	i := newTestIntegration(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_secret": map[string]any{
				"value":      "ek_legacy_xyz",
				"expires_at": float64(111),
			},
		})
	})
	nodes, err := i.handleCreateClientSecret(context.Background(), map[string]any{"model": "gpt-realtime-2", "voice": "cedar"}, 0)
	if err != nil {
		t.Fatalf("handleCreateClientSecret: %v", err)
	}
	out := decodeNode(t, nodes)
	if out["value"] != "ek_legacy_xyz" {
		t.Errorf("value = %v, want ek_legacy_xyz", out["value"])
	}
	if cfg, _ := out["config"].(map[string]any); cfg["voice"] != "cedar" {
		t.Errorf("config.voice = %v, want cedar (arg override)", cfg["voice"])
	}
}

func TestCreateClientSecret_APIErrorSurfaces(t *testing.T) {
	i := newTestIntegration(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	if _, err := i.handleCreateClientSecret(context.Background(), map[string]any{}, 0); err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

func TestCreateClientSecret_MissingKey(t *testing.T) {
	i := New(func(_ context.Context, _ string) (string, error) {
		return "", http.ErrNoLocation
	}, nil)
	if _, err := i.handleCreateClientSecret(context.Background(), map[string]any{}, 0); err == nil {
		t.Fatal("expected error when no API key resolves, got nil")
	}
}
