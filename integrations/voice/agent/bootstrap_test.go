package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveVoiceAgentToken_DirectWins(t *testing.T) {
	env := envMap(map[string]string{"VOICE_AGENT_TOKEN": "operator-token"})
	tok, err := ResolveVoiceAgentToken(context.Background(), env, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "operator-token", tok)
}

func TestResolveVoiceAgentToken_NoPathConfigured(t *testing.T) {
	env := envMap(map[string]string{})
	_, err := ResolveVoiceAgentToken(context.Background(), env, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_AGENT_TOKEN is unset")
}

func TestResolveVoiceAgentToken_BootstrapSuccess(t *testing.T) {
	var gotReq bootstrapRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/node/bootstrap", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(bootstrapResponse{
			Success:    true,
			PlainToken: "minted-jwt",
			IdentityID: "v1:identity:identity:va",
			ExpiresAt:  "2099-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	env := envMap(map[string]string{
		"MEMQL_NODE_BOOTSTRAP_TOKEN":    "shared-secret",
		"IDENTITY_VERIFIER_BASE_URL":    srv.URL,
		"MEMQL_VOICE_AGENT_INSTANCE_ID": "va-local",
	})
	tok, err := ResolveVoiceAgentToken(context.Background(), env, srv.Client(), nil)
	require.NoError(t, err)
	assert.Equal(t, "minted-jwt", tok)
	assert.Equal(t, "voice_agent", gotReq.TokenClass)
	assert.Equal(t, "va-local", gotReq.InstanceID)
	assert.Equal(t, "Bootstrap shared-secret", gotAuth)
}

func TestResolveVoiceAgentToken_BootstrapRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(bootstrapResponse{
			Success:   false,
			Error:     "wrong secret",
			ErrorCode: "invalid_bootstrap_secret",
		})
	}))
	defer srv.Close()

	env := envMap(map[string]string{
		"MEMQL_NODE_BOOTSTRAP_TOKEN":    "bad",
		"IDENTITY_VERIFIER_BASE_URL":    srv.URL,
		"MEMQL_VOICE_AGENT_INSTANCE_ID": "va-local",
	})
	_, err := ResolveVoiceAgentToken(context.Background(), env, srv.Client(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_bootstrap_secret")
}

func TestResolveVoiceAgentToken_BootstrapMissingInstanceID(t *testing.T) {
	env := envMap(map[string]string{
		"MEMQL_NODE_BOOTSTRAP_TOKEN": "secret",
		"IDENTITY_VERIFIER_BASE_URL": "http://identity:8081",
		// MEMQL_VOICE_AGENT_INSTANCE_ID intentionally absent.
	})
	_, err := ResolveVoiceAgentToken(context.Background(), env, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MEMQL_VOICE_AGENT_INSTANCE_ID")
}

func TestResolveVoiceAgentToken_BootstrapMissingBaseURL(t *testing.T) {
	env := envMap(map[string]string{
		"MEMQL_NODE_BOOTSTRAP_TOKEN":    "secret",
		"MEMQL_VOICE_AGENT_INSTANCE_ID": "va-local",
		// IDENTITY_VERIFIER_BASE_URL intentionally absent.
	})
	_, err := ResolveVoiceAgentToken(context.Background(), env, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IDENTITY_VERIFIER_BASE_URL")
}
