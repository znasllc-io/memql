package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	// httptest.NewServer is plaintext (http://127.0.0.1:port); the
	// https-only guard (memql#626) would short-circuit before any
	// request reaches the fake. These tests assert request/response
	// behaviour, not transport security, so opt into the dev escape
	// hatch. The dedicated https-enforcement test sets it to "" itself.
	t.Setenv(envAllowInsecureNodeBootstrap, "1")
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
	// Connection-refused is retryable, so opt out of the retry budget
	// to keep this a single-shot assertion (otherwise it'd spin for the
	// default 90s window before giving up).
	t.Setenv("MEMQL_NODE_BOOTSTRAP_RETRY_BUDGET", "0")
	// This test exercises the transport-failure path against a plaintext
	// URL, so opt into the dev escape hatch -- otherwise the https-only
	// guard (memql#626) would short-circuit before the dial is attempted.
	t.Setenv("MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP", "1")
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

// TestMaybeBootstrapNodeToken_RefusesInsecureHTTP is the memql#626
// regression guard: with the dev escape hatch UNSET (the staging/prod
// state), a plaintext http:// IDENTITY_VERIFIER_BASE_URL must be
// refused before any request is dialed, so an accidental
// http://identity:8085 in a deploy manifest fails fast and loud at the
// node instead of firing a POST identity bounces with `403
// insecure_transport`. Catches a transport regression in CI.
func TestMaybeBootstrapNodeToken_RefusesInsecureHTTP(t *testing.T) {
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	// Explicitly clear the escape hatch to model the staging/prod state.
	t.Setenv(envAllowInsecureNodeBootstrap, "")
	// A reachable plaintext host -- the point is that the scheme guard
	// fires BEFORE any dial, so the token is never minted even though the
	// host would answer.
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", "http://identity:8085")

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "v1:cluster:node:bff-x", "bff")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "https end-to-end")
	assert.Contains(t, err.Error(), "refusing insecure")
}

// TestMaybeBootstrapNodeToken_AcceptsInsecureHTTPWithEscapeHatch
// confirms the dev escape hatch still works: with
// MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP=1 the plaintext URL is
// accepted (a local plain-HTTP dev cluster relies on this), so the guard
// is opt-out-able for local development without weakening the secure
// default. Uses an unreachable host + zero retry budget so the call
// gets past the scheme guard and fails at the transport layer, proving
// the scheme check did not short-circuit.
func TestMaybeBootstrapNodeToken_AcceptsInsecureHTTPWithEscapeHatch(t *testing.T) {
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	t.Setenv(envAllowInsecureNodeBootstrap, "1")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_RETRY_BUDGET", "0")
	t.Setenv("IDENTITY_VERIFIER_BASE_URL", "http://127.0.0.1:1")

	tok, ok, err := maybeBootstrapNodeToken(context.Background(), nil, "x", "bff")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, tok)
	// A transport error (not the scheme refusal) proves the guard let
	// the plaintext call through under the escape hatch.
	assert.NotContains(t, err.Error(), "refusing insecure")
}

// flakyBootstrapServer mints a token only after `failFirst` calls have
// returned `failStatus` (e.g. 503). Mirrors identity being briefly down
// mid-roll, then coming back Ready. Reports the total hit count so a test
// can assert how many attempts the client made.
func flakyBootstrapServer(t *testing.T, secret string, failFirst int, failStatus int, hits *int32) *httptest.Server {
	t.Helper()
	// Plaintext httptest server -- opt into the dev escape hatch so the
	// https-only guard (memql#626) doesn't short-circuit the retry path
	// these tests exercise.
	t.Setenv(envAllowInsecureNodeBootstrap, "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		if int(n) <= failFirst {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":   false,
				"error":     "identity not ready",
				"errorCode": "starting",
			})
			return
		}
		var body struct {
			NodeId   string `json:"nodeId"`
			NodeType string `json:"nodeType"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = secret // secret validation covered elsewhere; this server focuses on transience
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

// TestBootstrapWithRetry_TransientThenSucceeds asserts the core #542 fix:
// when identity returns a 5xx for the first couple of attempts (briefly
// down mid-roll) and then succeeds, the client retries on backoff and
// captures the token rather than coming up tokenless.
func TestBootstrapWithRetry_TransientThenSucceeds(t *testing.T) {
	var hits int32
	srv := flakyBootstrapServer(t, "secret", 2 /*fail first 2*/, http.StatusServiceUnavailable, &hits)

	tok, err := bootstrapWithRetry(context.Background(), nil, srv.URL, "secret",
		"v1:cluster:node:agent-x", "agent",
		5*time.Second /*budget*/, time.Millisecond /*fast backoff*/)
	require.NoError(t, err)
	assert.Equal(t, "stub.jwt.token-for-agent-v1:cluster:node:agent-x", tok)
	assert.EqualValues(t, 3, atomic.LoadInt32(&hits), "should have taken 2 failed + 1 successful attempt")
}

// TestBootstrapWithRetry_GivesUpAfterBudget asserts that when identity
// stays down for the whole window the client eventually gives up with a
// clear error (and the caller's logs-and-proceeds path preserves the old
// tokenless behaviour -- no regression vs the single-attempt original).
func TestBootstrapWithRetry_GivesUpAfterBudget(t *testing.T) {
	var hits int32
	srv := flakyBootstrapServer(t, "secret", 1<<30 /*always fail*/, http.StatusServiceUnavailable, &hits)

	tok, err := bootstrapWithRetry(context.Background(), nil, srv.URL, "secret",
		"x", "bff", 30*time.Millisecond /*budget*/, time.Millisecond)
	require.Error(t, err)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "giving up")
	assert.Greater(t, atomic.LoadInt32(&hits), int32(1), "should have retried more than once within the budget")
}

// TestBootstrapWithRetry_PermanentNotRetried asserts a 4xx rejection
// (wrong secret / bad request) short-circuits immediately -- a retry
// won't fix a misconfig, so we must not spin for the whole budget.
func TestBootstrapWithRetry_PermanentNotRetried(t *testing.T) {
	var hits int32
	srv := flakyBootstrapServer(t, "secret", 1<<30, http.StatusUnauthorized, &hits)

	tok, err := bootstrapWithRetry(context.Background(), nil, srv.URL, "secret",
		"x", "bff", 10*time.Second /*generous budget*/, time.Millisecond)
	require.Error(t, err)
	assert.Empty(t, tok)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits), "a 4xx is permanent; must not retry")
	assert.Contains(t, err.Error(), "status=401")
}

// TestBootstrapWithRetry_ContextCancelCutsRetry asserts an already-cancelled
// context stops the retry loop promptly instead of waiting out the budget.
func TestBootstrapWithRetry_ContextCancelCutsRetry(t *testing.T) {
	var hits int32
	srv := flakyBootstrapServer(t, "secret", 1<<30, http.StatusServiceUnavailable, &hits)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before we start

	tok, err := bootstrapWithRetry(ctx, nil, srv.URL, "secret",
		"x", "bff", 10*time.Second, 50*time.Millisecond)
	require.Error(t, err)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "context cancelled")
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
