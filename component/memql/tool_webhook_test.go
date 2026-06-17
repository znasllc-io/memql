package memql

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestEngineForWebhook(allowedHosts ...string) *MemQLEngine {
	return &MemQLEngine{
		config: engineConfig{
			WebhookTimeoutSeconds: 5,
			WebhookAllowedHosts:   allowedHosts,
		},
	}
}

// hostOf returns an httptest server's host:port for allowlisting. httptest
// binds loopback (127.0.0.1), which the SSRF default-deny floor blocks unless
// explicitly allowlisted -- exactly how a real deployment wires an internal
// webhook target -- so the mechanics tests allowlist their test server.
func hostOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

// agentCtxForTest returns a context.Background that carries an
// acting-agent role -- ExecuteTool's universal agent-only enforcement
// rejects callers without one. Tests exercise the post-gate code
// path; the role string is a placeholder.
func agentCtxForTest() context.Context {
	return WithActingAgentRole(context.Background(), "specialist")
}

func TestExecuteWebhookBasicPOST(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected application/json content type, got %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"received":` + string(body) + `}`))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testWebhook",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/test",
			Method: "POST",
			Body: map[string]any{
				"message": "$args.task",
			},
		},
	}

	result, err := e.ExecuteTool(agentCtxForTest(), tool, map[string]any{
		"task": "hello world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "hello world") {
		t.Fatalf("expected response to contain 'hello world', got: %s", result.Content[0].Text)
	}
}

func TestExecuteWebhookGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testGet",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/health",
			Method: "GET",
		},
	}

	result, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result")
	}
	if !strings.Contains(result.Content[0].Text, `"status":"ok"`) {
		t.Fatalf("unexpected response: %s", result.Content[0].Text)
	}
}

func TestExecuteWebhookURLSubstitution(t *testing.T) {
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testURLSub",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/workspace/$args.workspace/files",
			Method: "GET",
		},
	}

	result, err := e.ExecuteTool(agentCtxForTest(), tool, map[string]any{
		"workspace": "agent-sofia",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result")
	}
	if receivedPath != "/workspace/agent-sofia/files" {
		t.Fatalf("expected /workspace/agent-sofia/files, got %s", receivedPath)
	}
}

func TestExecuteWebhookBodySubstitution(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testBodySub",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL,
			Method: "POST",
			Body: map[string]any{
				"command":   "$args.task",
				"workspace": "$args.workspace",
				"nested": map[string]any{
					"value": "$args.task",
				},
			},
		},
	}

	_, err := e.ExecuteTool(agentCtxForTest(), tool, map[string]any{
		"task":      "write a script",
		"workspace": "/workspaces/sofia",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["command"] != "write a script" {
		t.Fatalf("expected command='write a script', got %v", receivedBody["command"])
	}
	if receivedBody["workspace"] != "/workspaces/sofia" {
		t.Fatalf("expected workspace='/workspaces/sofia', got %v", receivedBody["workspace"])
	}
	nested, _ := receivedBody["nested"].(map[string]any)
	if nested == nil || nested["value"] != "write a script" {
		t.Fatalf("expected nested.value='write a script', got %v", nested)
	}
}

func TestExecuteWebhookHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testError",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL,
			Method: "POST",
		},
	}

	result, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true for 500 response")
	}
	if !strings.Contains(result.Content[0].Text, "500") {
		t.Fatalf("expected status 500 in error text, got: %s", result.Content[0].Text)
	}
}

func TestExecuteWebhookCustomHeaders(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testHeaders",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL,
			Method: "GET",
			Headers: map[string]string{
				"Authorization": "Bearer test-token",
			},
		},
	}

	_, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAuth != "Bearer test-token" {
		t.Fatalf("expected auth header 'Bearer test-token', got %q", receivedAuth)
	}
}

func TestExecuteWebhookHostAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Extract host from test server URL.
	srvHost := strings.TrimPrefix(srv.URL, "http://")

	// Test allowed host.
	e := newTestEngineForWebhook(srvHost)
	tool := &Tool{
		Name: "testAllowed",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/test",
			Method: "GET",
		},
	}

	result, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error for allowed host: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result for allowed host")
	}

	// Test blocked host: a public host not on the allowlist is rejected before
	// any request is made. Uses a public IP literal (no DNS) that is NOT the
	// internal-network floor, so this exercises the allowlist-deny path
	// specifically (the SSRF floor is covered by TestValidateWebhookHostSSRF).
	e2 := newTestEngineForWebhook("nemoclaw:18789")
	tool2 := &Tool{
		Name: "testBlocked",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    "http://93.184.216.34/test",
			Method: "GET",
		},
	}

	_, err = e2.ExecuteTool(agentCtxForTest(), tool2, nil)
	if err == nil {
		t.Fatalf("expected error for blocked host, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed webhook hosts") {
		t.Fatalf("expected allowlist error, got: %v", err)
	}
}

// TestValidateWebhookHostSSRF covers the SSRF default-deny floor (memql#1561):
// loopback / link-local / metadata / private / mDNS targets are blocked even
// with NO allowlist, while public hosts pass and an explicit allowlist entry
// overrides the floor. Uses IP literals + localhost so it does no DNS.
func TestValidateWebhookHostSSRF(t *testing.T) {
	noAllow := newTestEngineForWebhook()

	blocked := []string{
		"http://127.0.0.1/x",
		"http://127.0.0.1:9000/x",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.5/x",
		"http://192.168.1.10/x",
		"http://172.16.0.1/x",
		"http://[::1]/x",
		"http://localhost/x",
		"http://foo.local/x",
		"http://0.0.0.0/x",
	}
	for _, u := range blocked {
		if err := noAllow.validateWebhookHost(u); err == nil {
			t.Errorf("expected %q blocked by SSRF floor (empty allowlist), got nil", u)
		}
	}

	// Public hosts (IP literals, no DNS) pass with no allowlist.
	for _, u := range []string{"http://93.184.216.34/x", "http://8.8.8.8/x"} {
		if err := noAllow.validateWebhookHost(u); err != nil {
			t.Errorf("expected public host %q allowed with empty allowlist, got %v", u, err)
		}
	}

	// Explicit allowlist overrides the floor (operator escape valve).
	allowLoopback := newTestEngineForWebhook("127.0.0.1:9000")
	if err := allowLoopback.validateWebhookHost("http://127.0.0.1:9000/x"); err != nil {
		t.Errorf("expected allowlisted loopback permitted, got %v", err)
	}
	// ...but a different internal host not on the allowlist is still blocked.
	if err := allowLoopback.validateWebhookHost("http://169.254.169.254/x"); err == nil {
		t.Errorf("expected non-allowlisted metadata IP blocked, got nil")
	}
}

func TestExecuteWebhookDefaultMethodPOST(t *testing.T) {
	var receivedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "testDefaultMethod",
		Handler: &ToolHandler{
			Type: "webhook",
			URL:  srv.URL,
			// Method intentionally empty -- should default to POST.
		},
	}

	_, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedMethod != "POST" {
		t.Fatalf("expected default method POST, got %s", receivedMethod)
	}
}

func TestExecuteWebhookNoURL(t *testing.T) {
	e := newTestEngineForWebhook()
	tool := &Tool{
		Name: "testNoURL",
		Handler: &ToolHandler{
			Type: "webhook",
			URL:  "",
		},
	}

	_, err := e.ExecuteTool(agentCtxForTest(), tool, nil)
	if err == nil {
		t.Fatalf("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "has no URL") {
		t.Fatalf("expected 'has no URL' error, got: %v", err)
	}
}
