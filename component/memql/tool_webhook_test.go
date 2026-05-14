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

	e := newTestEngineForWebhook()
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

	result, err := e.ExecuteTool(context.Background(), tool, map[string]any{
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

	e := newTestEngineForWebhook()
	tool := &Tool{
		Name: "testGet",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/health",
			Method: "GET",
		},
	}

	result, err := e.ExecuteTool(context.Background(), tool, nil)
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

	e := newTestEngineForWebhook()
	tool := &Tool{
		Name: "testURLSub",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/workspace/$args.workspace/files",
			Method: "GET",
		},
	}

	result, err := e.ExecuteTool(context.Background(), tool, map[string]any{
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

	e := newTestEngineForWebhook()
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

	_, err := e.ExecuteTool(context.Background(), tool, map[string]any{
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

	e := newTestEngineForWebhook()
	tool := &Tool{
		Name: "testError",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL,
			Method: "POST",
		},
	}

	result, err := e.ExecuteTool(context.Background(), tool, nil)
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

	e := newTestEngineForWebhook()
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

	_, err := e.ExecuteTool(context.Background(), tool, nil)
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

	result, err := e.ExecuteTool(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error for allowed host: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result for allowed host")
	}

	// Test blocked host.
	e2 := newTestEngineForWebhook("nemoclaw:18789")
	tool2 := &Tool{
		Name: "testBlocked",
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/test",
			Method: "GET",
		},
	}

	_, err = e2.ExecuteTool(context.Background(), tool2, nil)
	if err == nil {
		t.Fatalf("expected error for blocked host, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed webhook hosts") {
		t.Fatalf("expected allowlist error, got: %v", err)
	}
}

func TestExecuteWebhookDefaultMethodPOST(t *testing.T) {
	var receivedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook()
	tool := &Tool{
		Name: "testDefaultMethod",
		Handler: &ToolHandler{
			Type: "webhook",
			URL:  srv.URL,
			// Method intentionally empty -- should default to POST.
		},
	}

	_, err := e.ExecuteTool(context.Background(), tool, nil)
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

	_, err := e.ExecuteTool(context.Background(), tool, nil)
	if err == nil {
		t.Fatalf("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "has no URL") {
		t.Fatalf("expected 'has no URL' error, got: %v", err)
	}
}
