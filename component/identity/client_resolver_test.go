package identity

import (
	"context"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// oauthClientNode builds a MemoryNode shaped like oauthClientFull.
func oauthClientNode(clientId, redirectURIsJSON string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: clientId,
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"clientId":         structpb.NewStringValue(clientId),
			"redirectURIsJSON": structpb.NewStringValue(redirectURIsJSON),
		}},
	}
}

// resolverFakeEngine returns the given oauthClient nodes for any
// oAuthClientByClientId call; errors for everything else.
type resolverFakeEngine struct {
	nodes []*memqlv1.MemoryNode
}

func (f *resolverFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if strings.Contains(q, "oAuthClientByClientId(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.nodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func TestResolveClient_StaticFirst(t *testing.T) {
	cfg := Config{RegisteredClients: []RegisteredClient{
		{ClientId: "app", RedirectURIs: []string{"https://app.example.com/auth/callback"}},
	}}

	// Static-only resolution (store == nil) must still work.
	got := ResolveClient(context.Background(), cfg, nil, "app")
	if got == nil || got.ClientId != "app" {
		t.Fatalf("ResolveClient(static, nil store) = %+v, want app", got)
	}
	if !ClientAllowsRedirectURI(context.Background(), cfg, nil, "app", "https://app.example.com/auth/callback") {
		t.Fatalf("ClientAllowsRedirectURI should accept the registered static redirect")
	}
	if ClientAllowsRedirectURI(context.Background(), cfg, nil, "app", "https://evil.example/cb") {
		t.Fatalf("ClientAllowsRedirectURI should reject an unregistered redirect")
	}
}

func TestResolveClient_DBFallback(t *testing.T) {
	cfg := Config{} // no static clients
	store := &Store{Engine: &resolverFakeEngine{
		nodes: []*memqlv1.MemoryNode{
			oauthClientNode("mcp_abc123", `["https://claude.ai/api/mcp/auth_callback"]`),
		},
	}}

	got := ResolveClient(context.Background(), cfg, store, "mcp_abc123")
	if got == nil || got.ClientId != "mcp_abc123" {
		t.Fatalf("ResolveClient(DB) = %+v, want mcp_abc123", got)
	}
	if len(got.RedirectURIs) != 1 || got.RedirectURIs[0] != "https://claude.ai/api/mcp/auth_callback" {
		t.Fatalf("ResolveClient(DB) redirect uris = %v, want the registered one", got.RedirectURIs)
	}

	if !ClientAllowsRedirectURI(context.Background(), cfg, store, "mcp_abc123", "https://claude.ai/api/mcp/auth_callback") {
		t.Fatalf("ClientAllowsRedirectURI should accept the DB-registered redirect")
	}
	if ClientAllowsRedirectURI(context.Background(), cfg, store, "mcp_abc123", "https://claude.ai/other") {
		t.Fatalf("ClientAllowsRedirectURI should reject an unregistered DB redirect")
	}
}

func TestResolveClient_Unknown(t *testing.T) {
	cfg := Config{}
	store := &Store{Engine: &resolverFakeEngine{nodes: nil}}

	if got := ResolveClient(context.Background(), cfg, store, "nope"); got != nil {
		t.Fatalf("ResolveClient(unknown) = %+v, want nil", got)
	}
	if ClientAllowsRedirectURI(context.Background(), cfg, store, "nope", "https://x") {
		t.Fatalf("ClientAllowsRedirectURI(unknown) = true, want false")
	}
	// Empty clientId resolves to nil regardless of store.
	if got := ResolveClient(context.Background(), cfg, store, ""); got != nil {
		t.Fatalf("ResolveClient(\"\") = %+v, want nil", got)
	}
}

func TestResolveClient_LoopbackAnyPort_DB(t *testing.T) {
	cfg := Config{}
	store := &Store{Engine: &resolverFakeEngine{
		nodes: []*memqlv1.MemoryNode{
			oauthClientNode("mcp_loop", `["http://127.0.0.1/callback"]`),
		},
	}}
	// RFC 8252: a loopback redirect with no explicit port matches any port.
	if !ClientAllowsRedirectURI(context.Background(), cfg, store, "mcp_loop", "http://127.0.0.1:51823/callback") {
		t.Fatalf("ClientAllowsRedirectURI should accept loopback any-port for the DB client")
	}
}
