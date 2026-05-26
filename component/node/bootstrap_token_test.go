package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBootstrapServer spins up an httptest.Server that mirrors
// identity's POST /node/bootstrap surface. Lets the node-side
// bootstrap client run through its full request -> response cycle
// without depending on a real identity binary.
//
// The handler validates the Authorization header against
// `expectedSecret` (constant-time-like compare) and returns either
// the success envelope (echoing nodeId/nodeType, plus a synthetic
// JWT-shaped string) or the matching error envelope. The token
// is a static stub -- the node side doesn't validate it; that's
// the verifier's job downstream.
func fakeBootstrapServer(t *testing.T, expectedSecret string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Endpoint surface.
		if r.URL.Path != "/node/bootstrap" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bootstrap " + expectedSecret
		if expectedSecret == "" || got != want {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":   false,
				"error":     "invalid bootstrap secret",
				"errorCode": "invalid_bootstrap_secret",
			})
			return
		}
		var body struct {
			NodeId   string `json:"nodeId"`
			NodeType string `json:"nodeType"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":    true,
			"plainToken": "stub.jwt.token-for-" + body.NodeType + "-" + body.NodeId,
			"identityId": "v1:identity:identity:node:" + body.NodeType + ":" + body.NodeId,
			"nodeType":   body.NodeType,
			"nodeId":     body.NodeId,
			"expiresAt":  "2099-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMaybeBootstrapNodeToken_OperatorTokenWins asserts the
// operator-provisioned MEMQL_NODE_TOKEN always wins over the
// bootstrap path: when the env var is set, the function is a no-op
// even when the bootstrap secret + identity URL are also set.
// Protects the "production overrides dev" invariant.
func TestMaybeBootstrapNodeToken_OperatorTokenWins(t *testing.T) {
	srv := fakeBootstrapServer(t, "secret")
	t.Setenv("MEMQL_NODE_TOKEN", "operator-provisioned-jwt")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "v1:cluster:node:x", "bff")
	require.NoError(t, err)
	assert.False(t, ok, "should have been a no-op (operator token wins)")
	assert.Empty(t, tok)
}

// TestMaybeBootstrapNodeToken_NotOptedIn asserts the function is
// a no-op when MEMQL_NODE_BOOTSTRAP_TOKEN is unset. Catches an
// accidental change that would make every node hit identity at
// boot even when the operator hasn't opted in.
func TestMaybeBootstrapNodeToken_NotOptedIn(t *testing.T) {
	srv := fakeBootstrapServer(t, "secret")
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "x", "bff")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
}

// TestMaybeBootstrapNodeToken_GoldenPath asserts the happy path:
// empty operator token + bootstrap secret + identity URL all set
// -> POST to /node/bootstrap returns success -> token is captured
// and surfaced to the caller.
func TestMaybeBootstrapNodeToken_GoldenPath(t *testing.T) {
	const secret = "shared-dev-secret"
	srv := fakeBootstrapServer(t, secret)

	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", secret)
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "v1:cluster:node:bff-local", "bff")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "stub.jwt.token-for-bff-v1:cluster:node:bff-local", tok)
}

// TestMaybeBootstrapNodeToken_WrongSecret asserts a 401 from
// identity surfaces as an error with the structured code from
// the response body, so operators can debug without grepping
// identity logs.
func TestMaybeBootstrapNodeToken_WrongSecret(t *testing.T) {
	srv := fakeBootstrapServer(t, "the-right-secret")

	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "the-wrong-secret")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "x", "bff")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "invalid_bootstrap_secret")
	assert.Contains(t, err.Error(), "status=401")
}

// TestMaybeBootstrapNodeToken_NoIdentityURL asserts that opting
// in via MEMQL_NODE_BOOTSTRAP_TOKEN but forgetting
// IDENTITY_VERIFIER_BASE_URL surfaces a clear error rather than
// a nil-deref / silent skip.
func TestMaybeBootstrapNodeToken_NoIdentityURL(t *testing.T) {
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", "")

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "x", "bff")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "IDENTITY_VERIFIER_BASE_URL is empty")
}

// TestMaybeBootstrapNodeToken_UnreachableIdentity asserts that a
// connection-level failure surfaces clearly. Uses an httptest
// server URL whose host is unreachable -- gets a network error.
func TestMaybeBootstrapNodeToken_UnreachableIdentity(t *testing.T) {
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	// 127.0.0.1:1 is a port no server listens on; connection
	// refused expected.
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", "http://127.0.0.1:1")

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "x", "bff")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
	// We don't pin the exact message (varies by platform) but the
	// error should reference the POST or the URL.
	assert.True(t,
		strings.Contains(err.Error(), "127.0.0.1:1") || strings.Contains(err.Error(), "POST"),
		"error should reference target URL or POST verb: %v", err)
}

// TestIdentity_EnsureBearerToken_DoesNothingWhenTokenPresent
// asserts the EnsureBearerToken wrapper is a no-op when the
// Identity already carries a BearerToken (operator-provisioned).
func TestIdentity_EnsureBearerToken_DoesNothingWhenTokenPresent(t *testing.T) {
	id := &Identity{
		ID:          "v1:cluster:node:bff-test",
		Type:        NodeTypeBFF,
		BearerToken: "preexisting-operator-token",
	}
	// Set bootstrap env so we can detect if EnsureBearerToken
	// improperly overrides.
	srv := fakeBootstrapServer(t, "secret")
	t.Setenv("MEMQL_NODE_TOKEN", "preexisting-operator-token")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	require.NoError(t, id.EnsureBearerToken(context.Background(), nil))
	assert.Equal(t, "preexisting-operator-token", id.BearerToken)
}

// TestIdentity_EnsureBearerToken_PopulatesViaBootstrap asserts
// the wrapper invokes the bootstrap path and writes the result
// to Identity.BearerToken when the preconditions line up.
func TestIdentity_EnsureBearerToken_PopulatesViaBootstrap(t *testing.T) {
	const secret = "dev-secret-x"
	srv := fakeBootstrapServer(t, secret)

	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", secret)
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", srv.URL)

	id := &Identity{
		ID:   "v1:cluster:node:cognition-test",
		Type: NodeTypeCognition,
	}
	require.NoError(t, id.EnsureBearerToken(context.Background(), nil))
	assert.Equal(t, "stub.jwt.token-for-cognition-v1:cluster:node:cognition-test", id.BearerToken)
}
