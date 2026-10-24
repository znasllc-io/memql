package appsession

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInferenceConfigHasNoMCPServerOrCredential(t *testing.T) {
	data, err := codexMCPBody("https://unavailable.example/mcp", "secret-session-bearer", sessionMCPSettings{InferenceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mcp_servers") || strings.Contains(string(data), "secret-session-bearer") {
		t.Fatalf("model call configured MCP: %s", data)
	}
	if !strings.Contains(string(data), "enabled = false") {
		t.Fatal("lost network boundary")
	}
}

func TestInferenceSessionRunsWithoutMCPButDelegatedSessionStillRequiresIt(t *testing.T) {
	fakeApp(t, "claude", `echo '{"type":"result","result":"Hello","session_id":"inference"}'`)
	for _, inference := range []bool{true, false} {
		t.Run(map[bool]string{true: "inference", false: "delegated"}[inference], func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			h := newRig(t)
			h.manager.opts.HTTPClient = http.DefaultClient
			end := h.start(t, func(start *memqlv1.AppSessionStart) { start.InferenceOnly = inference; start.McpEndpoint = server.URL })
			if inference {
				if end.GetExitCode() != 0 || requests.Load() != 0 {
					t.Fatalf("model call depended on MCP: end=%v requests=%d", end, requests.Load())
				}
			} else if end.GetExitCode() == 0 || requests.Load() == 0 {
				t.Fatalf("delegated session bypassed MCP readiness: %v", end)
			}
		})
	}
}
