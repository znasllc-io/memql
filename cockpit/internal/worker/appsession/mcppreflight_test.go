package appsession

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestMCPPreflightAuthenticatesAndClosesItsSession(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testBearer {
			t.Error("missing session credential")
		}
		if r.Method == http.MethodDelete {
			methods = append(methods, "DELETE")
			w.WriteHeader(204)
			return
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		methods = append(methods, req.Method)
		var result any
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "preflight")
			result = map[string]any{"protocolVersion": "2025-06-18"}
		} else {
			if r.Header.Get("Mcp-Session-Id") != "preflight" {
				t.Error("missing negotiated session")
			}
			result = map[string]any{"tools": []map[string]string{{"name": "submit"}, {"name": "executeCapability"}, {"name": "next_task"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	names, err := preflightMCP(context.Background(), server.Client(), server.URL, testBearer)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"executeCapability", "next_task", "submit"}) {
		t.Fatal(names)
	}
	if !reflect.DeepEqual(methods, []string{"initialize", "notifications/initialized", "tools/list", "DELETE"}) {
		t.Fatal(methods)
	}
}

func TestMCPPreflightFailureDoesNotLaunchTheApp(t *testing.T) {
	h := newRig(t)
	marker := filepath.Join(t.TempDir(), "launched")
	fakeApp(t, "claude", "touch "+marker+"\nexit 0\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer server.Close()
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.McpEndpoint = server.URL })
	if !strings.Contains(end.GetError(), "MCP readiness") {
		t.Fatalf("unexpected result: %+v", end)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("paid app process started without tools")
	}
	if _, err := os.Stat(filepath.Join(h.workspace, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatal("preflight failure left credential config")
	}
}

func TestCodexSessionGrantsAndBoundsSurviveRenewal(t *testing.T) {
	settings := sessionMCPSettings{Tools: []string{"next_task", "submit", "executeCapability"}, DenyPaths: []string{"/private/worker-state"}, RuntimePaths: []string{"/opt/codex/bin/codex"}}
	config, err := writeMCPConfig(apps.IDCodex, t.TempDir(), "http://127.0.0.1/mcp", testBearer, "scoped", t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Remove()
	for _, credential := range []string{testBearer, "renewed-bearer"} {
		if err := config.Renew(credential); err != nil {
			t.Fatal(err)
		}
		path, _ := config.paths()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		for _, fragment := range []string{`default_permissions = "memql_session"`, `required = true`, `enabled_tools = ["next_task","submit","executeCapability"]`, `[mcp_servers.memql.tools."executeCapability"]`, `approval_mode = "approve"`, `":root" = "deny"`, `"/opt/codex/bin/codex" = "read"`, `":minimal" = "read"`, `"/private/worker-state" = "deny"`, `".memql-session" = "deny"`, `enabled = false`, "Bearer " + credential} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("missing config fragment %s", fragment)
			}
		}
		if strings.Contains(body, "default_tools_approval_mode") {
			t.Fatal("unbounded tool grant")
		}
	}
}
