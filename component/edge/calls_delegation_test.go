package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestAgentCredentialIsKeptToExactAgentRoute(t *testing.T) {
	for _, path := range []string{"/_memql/calls/agent", "/_memql/calls/account", "/_memql/calls/stripe-webhook"} {
		t.Run(path, func(t *testing.T) {
			var got http.Header
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone(); w.WriteHeader(200) }))
			defer upstream.Close()
			h := callsHandler(t, callsSite(nil), upstream.URL)
			req := accessRequest(http.MethodPost, path, `{"action":"balance"}`)
			req.Header.Set("Authorization", "Bearer agent-secret")
			req.Header.Set(memql.CallsAgentCredentialHeader, "forged-stamp")
			req.Header.Set("Cookie", memql.CallsCookieName+"=human-session; other=secret")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			if got.Get("Authorization") != "" {
				t.Fatal("machine credential reached identity bearer middleware")
			}
			if path == "/_memql/calls/agent" {
				if got.Get(memql.CallsAgentCredentialHeader) != "agent-secret" || got.Get("Cookie") != "" {
					t.Fatal("wrong agent boundary", got)
				}
			} else if got.Get(memql.CallsAgentCredentialHeader) != "" {
				t.Fatal("agent authority leaked to another route")
			}
			if strings.Contains(path, "webhook") && got.Get("Cookie") != "" {
				t.Fatal("webhook received human session")
			}
		})
	}
}
func TestProviderWebhookPreservesRawBodyAndAcceptsLargerDecision(t *testing.T) {
	body := `{"payload":"` + strings.Repeat("a", 12000) + `"}`
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		if r.Header.Get("X-Signature-V2") != "signature" {
			t.Error("signature dropped")
		}
	}))
	defer upstream.Close()
	h := callsHandler(t, callsSite(nil), upstream.URL)
	req := accessRequest(http.MethodPost, "/_memql/calls/didit-webhook", body)
	req.Header.Set("X-Signature-V2", "signature")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || got != body {
		t.Fatalf("raw decision not forwarded, status %d", rec.Code)
	}
}
