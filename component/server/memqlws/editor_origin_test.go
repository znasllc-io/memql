package memqlws

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Exercise the real HTTP upgrade and gRPC hop: accepting the origin in the
// auth middleware alone does not mean the WebSocket library will accept it.
func TestEditorOriginUpgradeAndStream(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	memqlv1.RegisterMemqlServiceServer(server, &stubMemqlService{})
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	handler, err := New(Options{Endpoint: lis.Addr().String(), OriginPatterns: []string{"os.memql.localhost"}})
	require.NoError(t, err)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{"role": "user", "sub": "editor-user"}
		handler.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
	}))
	defer app.Close()

	for _, tc := range []struct {
		origin  string
		allowed bool
	}{
		{"https://vscode.memql.localhost", true},
		{"https://vscode.dev", true},
		{"https://v--01234567890123456789.vscode-cdn.net", true},
		{"http://vscode.memql.localhost", false},
		{"https://vscode.memql.localhost:444", false},
		{"https://vscode--assets--01234567890123456789.memql.localhost", false},
		{"https://vscode.memql.localhost.attacker.example", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(app.URL, "http"), &websocket.DialOptions{
				HTTPHeader:   http.Header{"Origin": []string{tc.origin}},
				Subprotocols: []string{"bearer", "test-token"},
			})
			if !tc.allowed {
				require.Error(t, err)
				require.NotNil(t, response)
				require.Equal(t, http.StatusForbidden, response.StatusCode)
				return
			}
			require.NoError(t, err)
			defer conn.Close(websocket.StatusNormalClosure, "done")
			require.Equal(t, "bearer", conn.Subprotocol())
			require.NoError(t, wsjson.Write(ctx, conn, map[string]any{
				"messageId": "editor-query", "executeQuery": map[string]any{"requestId": "editor-query", "query": "concept==v1:demo"},
			}))
			var result map[string]any
			require.NoError(t, wsjson.Read(ctx, conn, &result))
			require.Equal(t, "editor-query", result["queryResult"].(map[string]any)["requestId"])
		})
	}
}
