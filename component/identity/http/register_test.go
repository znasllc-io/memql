package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// registerFakeEngine records mutationCreateOAuthClient calls and serves
// the persisted row back on a subsequent queryOAuthClientByClientId, so
// a test can register then resolve through one fake.
type registerFakeEngine struct {
	mu      sync.Mutex
	created map[string]*memqlv1.MemoryNode // clientId -> node
}

func newRegisterFakeEngine() *registerFakeEngine {
	return &registerFakeEngine{created: map[string]*memqlv1.MemoryNode{}}
}

func (f *registerFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "mutationCreateOAuthClient("):
		clientId := extractField(q, "clientId")
		uris := extractField(q, "redirectURIsJSON")
		f.created[clientId] = &memqlv1.MemoryNode{
			Id: clientId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"clientId":         structpb.NewStringValue(clientId),
				"redirectURIsJSON": structpb.NewStringValue(uris),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	case strings.HasPrefix(q, "queryOAuthClientByClientId("):
		clientId := extractField(q, "clientId")
		if n, ok := f.created[clientId]; ok {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{n}}}, nil
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// extractField pulls a `"key":"value"` or `key: "value"` string out of a
// serialized mutation/query call. Good enough for the test fake's
// single-level string args.
func extractField(q, key string) string {
	for _, pat := range []string{`"` + key + `":"`, key + `: "`} {
		i := strings.Index(q, pat)
		if i < 0 {
			continue
		}
		rest := q[i+len(pat):]
		// unescape the two sequences writeKVString produces (\" and \\)
		var b strings.Builder
		for j := 0; j < len(rest); j++ {
			if rest[j] == '\\' && j+1 < len(rest) {
				b.WriteByte(rest[j+1])
				j++
				continue
			}
			if rest[j] == '"' {
				break
			}
			b.WriteByte(rest[j])
		}
		return b.String()
	}
	return ""
}

func newRegisterTestServer(dcrEnabled bool) (*Server, *registerFakeEngine) {
	eng := newRegisterFakeEngine()
	return &Server{
		Cfg:   identity.Config{OAuthDCREnabled: dcrEnabled},
		Store: &identity.Store{Engine: eng},
	}, eng
}

func postRegister(t *testing.T, s *Server, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleRegister(rec, req)
	return rec
}

func TestHandleRegister_Valid(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if !strings.HasPrefix(resp.ClientId, "mcp_") {
		t.Fatalf("client_id = %q, want mcp_ prefix", resp.ClientId)
	}
	if resp.TokenEndpointAuthMethod != "none" {
		t.Fatalf("token_endpoint_auth_method = %q, want none", resp.TokenEndpointAuthMethod)
	}
	if resp.ClientIdIssuedAt == 0 {
		t.Fatalf("client_id_issued_at should be set")
	}

	// The returned client_id must resolve and accept its registered
	// redirect via the unified resolver.
	if identity.ResolveClient(context.Background(), s.Cfg, s.Store, resp.ClientId) == nil {
		t.Fatalf("registered client_id %q does not resolve", resp.ClientId)
	}
	if !identity.ClientAllowsRedirectURI(context.Background(), s.Cfg, s.Store, resp.ClientId, "https://claude.ai/api/mcp/auth_callback") {
		t.Fatalf("registered redirect_uri should be allowed for %q", resp.ClientId)
	}
}

func TestHandleRegister_NonHTTPSNonLoopbackRejected(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"redirect_uris":["http://evil.example/callback"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_redirect_uri" {
		t.Fatalf("error = %q, want invalid_redirect_uri", got)
	}
}

func TestHandleRegister_EmptyRedirectURIs(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"redirect_uris":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_redirect_uri" {
		t.Fatalf("error = %q, want invalid_redirect_uri", got)
	}
}

func TestHandleRegister_LoopbackHTTPAccepted(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"redirect_uris":["http://127.0.0.1:8080/cb","http://localhost/cb"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRegister_NonPublicAuthMethodRejected(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"redirect_uris":["https://x.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_client_metadata" {
		t.Fatalf("error = %q, want invalid_client_metadata", got)
	}
}

func TestHandleRegister_Disabled(t *testing.T) {
	s, eng := newRegisterTestServer(false)
	rec := postRegister(t, s, `{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "registration_disabled" {
		t.Fatalf("error = %q, want registration_disabled", got)
	}
	if len(eng.created) != 0 {
		t.Fatalf("disabled DCR must not persist any client; got %d", len(eng.created))
	}
}

func TestHandleRegister_TooManyRedirectURIs(t *testing.T) {
	s, _ := newRegisterTestServer(true)
	rec := postRegister(t, s, `{"redirect_uris":["https://a/1","https://a/2","https://a/3","https://a/4","https://a/5","https://a/6"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_redirect_uri" {
		t.Fatalf("error = %q, want invalid_redirect_uri", got)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, string(body))
	}
	s, _ := m["error"].(string)
	return s
}
