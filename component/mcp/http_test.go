package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestHandler builds the full /mcp HTTP handler (auth wrapper + mux) the
// same way NewHTTPDependency does, but with a stub auth middleware injected so
// the wiring can be exercised without a live JWKS verifier. The stub mirrors
// the real verifier.HTTPMiddleware contract: /health* paths bypass; every
// other path requires a Bearer Authorization header (otherwise 401).
func newTestHandler(t *testing.T, base Config) http.Handler {
	t.Helper()
	dep, err := NewHTTPDependency(HTTPConfig{
		Logger:     nil,
		BaseConfig: base,
		NewServer: func(c Config) *Server {
			return NewServer(nil, "memql-mcp", "9.9.9", nil, c)
		},
		authMiddleware: stubAuthMiddleware,
	})
	if err != nil {
		t.Fatalf("NewHTTPDependency: %v", err)
	}
	return dep.(*httpDependency).srv.Handler
}

// stubAuthMiddleware stands in for verifier.HTTPMiddleware in tests: it
// default-denies any non-health request lacking a Bearer token.
func stubAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/health") || strings.HasPrefix(p, "/readyz") || strings.HasPrefix(p, "/livez") {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func post(t *testing.T, h http.Handler, body, sessionID string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, MCPPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHTTPSessionRoundTrip drives the full Streamable HTTP path: initialize ->
// session id assigned -> tools/list against that session succeeds.
func TestHTTPSessionRoundTrip(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})

	// initialize: assigns a session id.
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	sessionID := rec.Header().Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("initialize did not assign an Mcp-Session-Id")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("initialize content-type = %q, want application/json", ct)
	}
	var initResp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("initialize body not JSON-RPC: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize error: %+v", initResp.Error)
	}

	// tools/list against the established session.
	rec = post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("tools/list body not JSON-RPC: %v", err)
	}
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %+v", listResp.Error)
	}
	m, ok := listResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list result not an object: %T", listResp.Result)
	}
	if _, ok := m["tools"]; !ok {
		t.Fatalf("tools/list result missing tools key: %v", m)
	}
}

// TestHTTPUnauthenticatedRejected asserts the endpoint is default-deny: a
// request with no bearer token never reaches a Server.
func TestHTTPUnauthenticatedRejected(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "", false /* no auth */)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated initialize status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(sessionHeader) != "" {
		t.Fatal("unauthenticated request should not be assigned a session id")
	}
}

// TestHTTPUnknownSession asserts a post against a non-existent session id is
// rejected with 404 (so the client re-initializes), not silently served.
func TestHTTPUnknownSession(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "deadbeef-not-a-session", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-session status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHTTPMissingSession asserts a non-initialize POST with no session header
// is a 400 with a JSON-RPC error envelope.
func TestHTTPMissingSession(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-session status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHTTPHealthBypassesAuth asserts the probe endpoints are reachable without
// a token (so k8s probes targeting this port work).
func TestHTTPHealthBypassesAuth(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	for _, path := range []string{"/healthz", "/readyz", "/livez"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

// TestHTTPNotificationAccepted asserts a notification (no id) is acked with 202
// and no body.
func TestHTTPNotificationAccepted(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "", true)
	sessionID := rec.Header().Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("no session id from initialize")
	}
	rec = post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sessionID, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("notification should have empty body, got %q", rec.Body.String())
	}
}

// TestHTTPDeleteSession asserts DELETE tears the session down; a follow-up POST
// then 404s.
func TestHTTPDeleteSession(t *testing.T) {
	h := newTestHandler(t, Config{Tier: TierAuthoring})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "", true)
	sessionID := rec.Header().Get(sessionHeader)

	req := httptest.NewRequest(http.MethodDelete, MCPPath, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(sessionHeader, sessionID)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, req)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delRec.Code)
	}

	rec = post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post after delete status = %d, want 404", rec.Code)
	}
}

// TestNewHTTPDependencyRequiresVerifier asserts the construction-time
// default-deny: no verifier (and no auth override) is a hard error.
func TestNewHTTPDependencyRequiresVerifier(t *testing.T) {
	_, err := NewHTTPDependency(HTTPConfig{
		NewServer: func(c Config) *Server { return NewServer(nil, "x", "0", nil, c) },
		// Verifier nil, authMiddleware nil.
	})
	if err == nil {
		t.Fatal("expected error when no verifier and no auth override is supplied")
	}
	if !strings.Contains(err.Error(), "verifier is required") {
		t.Fatalf("error = %q, want it to mention the verifier requirement", err.Error())
	}
}

// TestDispatch exercises the transport-agnostic Dispatch export directly: a
// request returns marshaled bytes; a notification returns no reply.
func TestDispatch(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "9.9.9", nil, Config{})

	raw, has := s.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
	if !has {
		t.Fatal("ping should produce a reply")
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("dispatch reply not JSON-RPC: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("ping error: %+v", resp.Error)
	}
	if string(resp.ID) != "7" {
		t.Fatalf("dispatch echoed id = %s, want 7", resp.ID)
	}

	if _, has := s.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); has {
		t.Fatal("notification should produce no reply")
	}
}

// TestConfigForRequestPins asserts the env pins win over claims when set.
func TestConfigForRequestPins(t *testing.T) {
	h := &httpHead{base: Config{Tier: TierSealed, ActingUser: "pinned-user", ActingRole: "reader"}}
	req := httptest.NewRequest(http.MethodPost, MCPPath, nil)
	cfg := h.configForRequest(req)
	if cfg.ActingUser != "pinned-user" || cfg.ActingRole != "reader" || cfg.Tier != TierSealed {
		t.Fatalf("pins not honored: %+v", cfg)
	}
}
