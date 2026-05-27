package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/nodetoken"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// newBootstrapTestServer assembles a Server with the bits
// handleNodeBootstrap actually reaches into (Cfg, Issuer, Logger). No
// HTTP listener -- we drive the handler directly with httptest.
func newBootstrapTestServer(t *testing.T, bootstrapSecret string) *Server {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())

	cfg := identity.Config{
		Enabled:            true,
		BaseURL:            "https://identity.test",
		JWTAudience:        "memql",
		KeyDir:             dir,
		NodeBootstrapToken: bootstrapSecret,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)

	return &Server{Cfg: cfg, Issuer: iss}
}

// driveBootstrapRequest is a httptest harness that issues a request
// against handleNodeBootstrap with the bootstrap dev escape hatch
// turned on (so the test doesn't need to fake TLS). Returns the
// recorded response.
func driveBootstrapRequest(t *testing.T, s *Server, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv(envAllowInsecureBootstrap, "1")
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/node/bootstrap", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bootstrap "+secret)
	}
	rec := httptest.NewRecorder()
	s.handleNodeBootstrap(rec, req)
	return rec
}

func decodeBootstrapResponse(t *testing.T, rec *httptest.ResponseRecorder) NodeBootstrapResponse {
	t.Helper()
	var resp NodeBootstrapResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp), "decode response body")
	return resp
}

// TestHandleNodeBootstrap_GoldenPath asserts: valid bootstrap secret
// + valid body -> 200 + non-empty plainToken that round-trips through
// the JWT verifier with the expected node-class claims.
func TestHandleNodeBootstrap_GoldenPath(t *testing.T) {
	const secret = "dev-bootstrap-secret-for-tests"
	s := newBootstrapTestServer(t, secret)

	body, err := json.Marshal(NodeBootstrapRequest{
		NodeId:   "v1:cluster:node:bff-local",
		NodeType: "bff",
	})
	require.NoError(t, err)

	rec := driveBootstrapRequest(t, s, secret, string(body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeBootstrapResponse(t, rec)
	assert.True(t, resp.Success)
	assert.NotEmpty(t, resp.PlainToken)
	assert.Equal(t, "v1:identity:identity:node:bff:v1:cluster:node:bff-local", resp.IdentityId)
	assert.Equal(t, "bff", resp.NodeType)
	assert.Equal(t, "v1:cluster:node:bff-local", resp.NodeId)
	assert.NotEmpty(t, resp.ExpiresAt)

	// Round-trip the returned JWT to confirm it's actually valid +
	// stamped with the expected claims. Catches "endpoint returns a
	// token, but it's malformed / wrong class" regressions.
	claims, err := s.Issuer.VerifyAccessToken(resp.PlainToken, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, identity.ClassNode, claims.Class)
	assert.Equal(t, "v1:cluster:node:bff-local", claims.NodeId)
	assert.Equal(t, "bff", claims.NodeType)
}

// TestHandleNodeBootstrap_DisabledWhenSecretUnset asserts that
// production deploys that leave MEMQL_NODE_BOOTSTRAP_TOKEN unset
// get a 503 "bootstrap_disabled" -- the endpoint stays dark.
func TestHandleNodeBootstrap_DisabledWhenSecretUnset(t *testing.T) {
	s := newBootstrapTestServer(t, "")

	body, err := json.Marshal(NodeBootstrapRequest{NodeId: "x", NodeType: "bff"})
	require.NoError(t, err)
	rec := driveBootstrapRequest(t, s, "any-secret-the-caller-tries", string(body))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.False(t, resp.Success)
	assert.Equal(t, "bootstrap_disabled", resp.ErrorCode)
	assert.Empty(t, resp.PlainToken)
}

// TestHandleNodeBootstrap_RejectsWrongSecret asserts a mismatched
// bootstrap header returns 401 + "invalid_bootstrap_secret" without
// leaking which side of the compare failed.
func TestHandleNodeBootstrap_RejectsWrongSecret(t *testing.T) {
	s := newBootstrapTestServer(t, "the-right-secret")

	body, err := json.Marshal(NodeBootstrapRequest{NodeId: "x", NodeType: "bff"})
	require.NoError(t, err)
	rec := driveBootstrapRequest(t, s, "wrong-secret", string(body))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.False(t, resp.Success)
	assert.Equal(t, "invalid_bootstrap_secret", resp.ErrorCode)
}

// TestHandleNodeBootstrap_RejectsMissingHeader asserts no
// Authorization header at all -> 401 (same code as wrong secret;
// the endpoint doesn't distinguish "missing" from "wrong" by
// design).
func TestHandleNodeBootstrap_RejectsMissingHeader(t *testing.T) {
	s := newBootstrapTestServer(t, "secret")

	body, err := json.Marshal(NodeBootstrapRequest{NodeId: "x", NodeType: "bff"})
	require.NoError(t, err)
	rec := driveBootstrapRequest(t, s, "", string(body))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.Equal(t, "invalid_bootstrap_secret", resp.ErrorCode)
}

// TestHandleNodeBootstrap_RejectsMissingFields asserts that the
// body must carry both nodeId and nodeType, and that missing
// either surfaces as a 400 bad_request (NOT a 500 from the
// downstream mint surface failing the same validation).
func TestHandleNodeBootstrap_RejectsMissingFields(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	cases := []struct {
		name string
		body NodeBootstrapRequest
	}{
		{"missing node id", NodeBootstrapRequest{NodeType: "bff"}},
		{"missing node type", NodeBootstrapRequest{NodeId: "v1:cluster:node:x"}},
		{"both missing", NodeBootstrapRequest{}},
		{"both whitespace", NodeBootstrapRequest{NodeId: "  ", NodeType: "\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			require.NoError(t, err)
			rec := driveBootstrapRequest(t, s, secret, string(body))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			resp := decodeBootstrapResponse(t, rec)
			assert.Equal(t, "bad_request", resp.ErrorCode)
		})
	}
}

// TestHandleNodeBootstrap_RejectsMalformedJSON asserts a body
// that doesn't parse as JSON surfaces as 400 bad_request.
func TestHandleNodeBootstrap_RejectsMalformedJSON(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	t.Setenv(envAllowInsecureBootstrap, "1")
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/node/bootstrap", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bootstrap "+secret)
	rec := httptest.NewRecorder()
	s.handleNodeBootstrap(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.Equal(t, "bad_request", resp.ErrorCode)
}

// TestHandleNodeBootstrap_RejectsPlaintextWithoutEscapeHatch
// asserts that production deploys (no
// MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP=1) get a 403
// "insecure_transport" when the bootstrap endpoint is hit over
// plain HTTP. The dev escape hatch is required to be explicit.
func TestHandleNodeBootstrap_RejectsPlaintextWithoutEscapeHatch(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	// Explicitly clear the escape hatch.
	t.Setenv(envAllowInsecureBootstrap, "")
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/node/bootstrap", strings.NewReader(`{"nodeId":"x","nodeType":"bff"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bootstrap "+secret)
	rec := httptest.NewRecorder()
	s.handleNodeBootstrap(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.Equal(t, "insecure_transport", resp.ErrorCode)
}

// TestHandleNodeBootstrap_AdmitsPlaintextWithXForwardedProto
// asserts that production deploys behind a TLS terminator (nginx
// / ingress / load-balancer) that sets X-Forwarded-Proto: https
// can hit /node/bootstrap without needing the dev escape hatch.
func TestHandleNodeBootstrap_AdmitsPlaintextWithXForwardedProto(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	// Clear the escape hatch -- want to confirm X-Forwarded-Proto
	// alone admits the request.
	t.Setenv(envAllowInsecureBootstrap, "")
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/node/bootstrap", strings.NewReader(`{"nodeId":"v1:cluster:node:x","nodeType":"bff"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bootstrap "+secret)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleNodeBootstrap(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// TestHandleNodeBootstrap_VoiceAgentGoldenPath asserts the
// memql#342 extension: tokenClass="voice_agent" + instanceId mints
// a class="voice_agent" JWT through the same /node/bootstrap
// endpoint + bootstrap secret.
func TestHandleNodeBootstrap_VoiceAgentGoldenPath(t *testing.T) {
	const secret = "dev-bootstrap-secret-for-tests"
	s := newBootstrapTestServer(t, secret)

	body, err := json.Marshal(NodeBootstrapRequest{
		TokenClass: "voice_agent",
		InstanceId: "voice-agent-local",
	})
	require.NoError(t, err)

	rec := driveBootstrapRequest(t, s, secret, string(body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeBootstrapResponse(t, rec)
	assert.True(t, resp.Success)
	assert.NotEmpty(t, resp.PlainToken)
	assert.Equal(t, "v1:identity:identity:voice_agent:voice-agent-local", resp.IdentityId)
	assert.Equal(t, "voice-agent-local", resp.NodeId, "voice-agent path echoes instance id via NodeId")
	assert.Empty(t, resp.NodeType, "voice-agent path doesn't carry NodeType")
	assert.NotEmpty(t, resp.ExpiresAt)

	// Round-trip the returned JWT to confirm it's actually valid +
	// stamped with class="voice_agent" (not class="node").
	claims, err := s.Issuer.VerifyAccessToken(resp.PlainToken, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, identity.ClassVoiceAgent, claims.Class)
	assert.Equal(t, "voice-agent-local", claims.NodeId, "voice-agent reuses NodeId claim slot for instance id")
}

// TestHandleNodeBootstrap_VoiceAgentRequiresInstanceId asserts that
// the voice-agent path rejects requests missing instanceId. The
// nodeId/nodeType fields are accepted (and ignored) on this path
// because the same bootstrap-token client struct serves both
// classes -- the parse can't always strip them upstream.
func TestHandleNodeBootstrap_VoiceAgentRequiresInstanceId(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	body := `{"tokenClass":"voice_agent"}`
	rec := driveBootstrapRequest(t, s, secret, body)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)
	assert.Equal(t, "bad_request", resp.ErrorCode)
	assert.Contains(t, resp.Error, "instanceId")
}

// TestHandleNodeBootstrap_RejectsUnknownTokenClass asserts that
// any tokenClass outside {"", "node", "voice_agent"} fails fast
// with a 400 -- a future class addition has to land server-side
// before clients can request it. The grammar stays exhaustive.
func TestHandleNodeBootstrap_RejectsUnknownTokenClass(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	body := `{"tokenClass":"admin","nodeId":"x","nodeType":"bff"}`
	rec := driveBootstrapRequest(t, s, secret, body)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)
	assert.Equal(t, "bad_request", resp.ErrorCode)
	assert.Contains(t, resp.Error, "tokenClass")
}

// TestHandleNodeBootstrap_EmptyTokenClassDefaultsToNode asserts
// backward compatibility: pre-#342 callers that omit tokenClass
// continue to get the class="node" mint path. This pins the
// "tokenClass is optional, defaults to node" contract so a future
// refactor can't accidentally make it required.
func TestHandleNodeBootstrap_EmptyTokenClassDefaultsToNode(t *testing.T) {
	const secret = "secret"
	s := newBootstrapTestServer(t, secret)

	// No tokenClass on the body -- existing #338 clients look like this.
	body := `{"nodeId":"v1:cluster:node:bff-local","nodeType":"bff"}`
	rec := driveBootstrapRequest(t, s, secret, body)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)
	require.NotEmpty(t, resp.PlainToken)
	claims, err := s.Issuer.VerifyAccessToken(resp.PlainToken, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, identity.ClassNode, claims.Class)
}

// fakeNodeTokenEngine is a recording stub for testing the
// row-persistence path memql#343 added. Captures every Execute
// call and (when lookupRow is non-nil) returns it as the response
// to queryNodeTokenByIdentityId so we can simulate "row already
// exists" or "row is revoked" paths.
type fakeNodeTokenEngine struct {
	calls []string
	// lookupRow, when non-nil, is returned by every Execute that
	// matches a queryNodeTokenByIdentityId. Lets tests stage "row
	// already exists" or "row is revoked".
	lookupRow *struct {
		IdentityId string
		Active     bool
		RevokedAt  string
	}
}

func (e *fakeNodeTokenEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	_ = ctx
	e.calls = append(e.calls, query)
	if strings.HasPrefix(query, "queryNodeTokenByIdentityId") && e.lookupRow != nil {
		creds, _ := structpb.NewStruct(map[string]any{
			"nodeId":    "x",
			"nodeType":  "bff",
			"revokedAt": e.lookupRow.RevokedAt,
		})
		payload := &structpb.Struct{Fields: map[string]*structpb.Value{
			"active":      structpb.NewBoolValue(e.lookupRow.Active),
			"credentials": structpb.NewStructValue(creds),
		}}
		return &memqlengine.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
				{Id: e.lookupRow.IdentityId, Payload: payload},
			}},
		}, nil
	}
	// Lookup-miss + every mutation: return empty result (success).
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// newBootstrapTestServerWithStore wires the same fixture as
// newBootstrapTestServer but additionally attaches a NodeTokenStore
// backed by the supplied fake engine so the persistence path runs.
func newBootstrapTestServerWithStore(t *testing.T, bootstrapSecret string, eng *fakeNodeTokenEngine) *Server {
	t.Helper()
	s := newBootstrapTestServer(t, bootstrapSecret)
	s.NodeTokenStore = &nodetoken.Store{Engine: eng}
	return s
}

// TestHandleNodeBootstrap_CreatesRowOnFirstCall asserts a bootstrap
// call against a node with no existing row issues a single
// mutationCreateNodeTokenIdentity with the canonical row id, system-
// user sentinel, and origin stamping (bootstrappedAt /
// bootstrappedFrom).
func TestHandleNodeBootstrap_CreatesRowOnFirstCall(t *testing.T) {
	const secret = "secret"
	eng := &fakeNodeTokenEngine{}
	s := newBootstrapTestServerWithStore(t, secret, eng)

	body := `{"nodeId":"v1:cluster:node:bff-local","nodeType":"bff"}`
	rec := driveBootstrapRequest(t, s, secret, body)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	require.GreaterOrEqual(t, len(eng.calls), 2, "calls=%v", eng.calls)
	assert.True(t, strings.HasPrefix(eng.calls[0], "queryNodeTokenByIdentityId"), "first call should be the lookup")
	createCallIdx := -1
	for i, c := range eng.calls {
		if strings.HasPrefix(c, "mutationCreateNodeTokenIdentity") {
			createCallIdx = i
			break
		}
	}
	require.NotEqual(t, -1, createCallIdx, "expected a mutationCreateNodeTokenIdentity call; calls=%v", eng.calls)
	create := eng.calls[createCallIdx]
	for _, fragment := range []string{
		`identityId:"v1:identity:identity:node:bff:v1:cluster:node:bff-local"`,
		`userId:"system:node_bootstrap"`,
		`mintedBy:"system:node_bootstrap"`,
		`nodeId:"v1:cluster:node:bff-local"`,
		`nodeType:"bff"`,
		`bootstrappedFrom:"`, // exact IP varies under httptest, but field must be present
		`bootstrappedAt:"`,
	} {
		assert.Contains(t, create, fragment, "mutation missing %q", fragment)
	}
}

// TestHandleNodeBootstrap_UpdatesRowOnRebootstrap asserts when the
// row already exists the handler issues mutationRecordNodeTokenBootstrap
// (update path), not another Create. The origin signals (bootstrappedAt
// + bootstrappedFrom) must NOT be re-passed -- the update path
// preserves them server-side; re-passing would corrupt the "first
// seen" signal.
func TestHandleNodeBootstrap_UpdatesRowOnRebootstrap(t *testing.T) {
	const secret = "secret"
	eng := &fakeNodeTokenEngine{
		lookupRow: &struct {
			IdentityId string
			Active     bool
			RevokedAt  string
		}{
			IdentityId: "v1:identity:identity:node:bff:v1:cluster:node:bff-local",
			Active:     true,
			RevokedAt:  "",
		},
	}
	s := newBootstrapTestServerWithStore(t, secret, eng)

	body := `{"nodeId":"v1:cluster:node:bff-local","nodeType":"bff"}`
	rec := driveBootstrapRequest(t, s, secret, body)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	updateCallIdx := -1
	for i, c := range eng.calls {
		if strings.HasPrefix(c, "mutationRecordNodeTokenBootstrap") {
			updateCallIdx = i
			break
		}
		assert.False(t, strings.HasPrefix(c, "mutationCreateNodeTokenIdentity"), "re-bootstrap must NOT issue a Create; calls=%v", eng.calls)
	}
	require.NotEqual(t, -1, updateCallIdx, "expected mutationRecordNodeTokenBootstrap; calls=%v", eng.calls)
	update := eng.calls[updateCallIdx]
	assert.Contains(t, update, `identityId:"v1:identity:identity:node:bff:v1:cluster:node:bff-local"`)
	assert.Contains(t, update, `lastBootstrappedAt:"`)
	// memql#343 v2: the whole-credentials replace fix REQUIRES every
	// preserved-origin field to appear (otherwise the variant-
	// discriminator validator rejects the update). The earlier
	// "must not appear" expectation was wrong -- pinning the correct
	// shape here.
	assert.Contains(t, update, "bootstrappedAt:")
	assert.Contains(t, update, "bootstrappedFrom:")
	assert.Contains(t, update, "nodeId:")
	assert.Contains(t, update, "nodeType:")
}

// TestHandleNodeBootstrap_RejectsRevokedRow asserts a bootstrap against
// a row with non-empty revokedAt is rejected with 403 node_token_revoked
// and no JWT is minted. The pre-mint gate prevents the verifier from
// having to reject a fresh credential the operator already revoked.
func TestHandleNodeBootstrap_RejectsRevokedRow(t *testing.T) {
	const secret = "secret"
	eng := &fakeNodeTokenEngine{
		lookupRow: &struct {
			IdentityId string
			Active     bool
			RevokedAt  string
		}{
			IdentityId: "v1:identity:identity:node:bff:v1:cluster:node:bff-local",
			Active:     false,
			RevokedAt:  "2026-05-26T12:00:00Z",
		},
	}
	s := newBootstrapTestServerWithStore(t, secret, eng)

	body := `{"nodeId":"v1:cluster:node:bff-local","nodeType":"bff"}`
	rec := driveBootstrapRequest(t, s, secret, body)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)
	assert.False(t, resp.Success)
	assert.Equal(t, "node_token_revoked", resp.ErrorCode)
	assert.Empty(t, resp.PlainToken, "must not mint a JWT for a revoked node")

	for _, c := range eng.calls {
		assert.False(t, strings.HasPrefix(c, "mutationCreateNodeTokenIdentity"), "revoked path must not Create; calls=%v", eng.calls)
		assert.False(t, strings.HasPrefix(c, "mutationRecordNodeTokenBootstrap"), "revoked path must not Record; calls=%v", eng.calls)
	}
}
