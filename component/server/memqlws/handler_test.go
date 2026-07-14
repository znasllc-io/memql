package memqlws

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestHandlerProxiesWebsocketMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	testSvc := &stubMemqlService{}
	memqlv1.RegisterMemqlServiceServer(grpcServer, testSvc)

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := New(Options{
		Logger:   logger,
		Endpoint: lis.Addr().String(),
	})
	require.NoError(t, err)

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memql/ws" {
			http.NotFound(w, r)
			return
		}
		claims := map[string]any{
			"role": "admin",
			"sub":  "tester",
		}
		ctx := auth.ContextWithClaims(r.Context(), claims)
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer app.Close()

	wsURL := "ws" + strings.TrimPrefix(app.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL+"/memql/ws", nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	requestId := uuid.NewString()
	execMsg := map[string]any{
		"messageId": requestId,
		"executeQuery": map[string]any{
			"requestId": requestId,
			"query":     "concept==v1:demo",
		},
	}

	require.NoError(t, wsjson.Write(ctx, conn, execMsg))

	require.Eventually(t, func() bool {
		testSvc.mu.Lock()
		defer testSvc.mu.Unlock()
		return len(testSvc.received) >= 2
	}, time.Second, 25*time.Millisecond, "memql service did not receive query")

	var response map[string]any
	require.NoError(t, wsjson.Read(ctx, conn, &response))

	resultPayload, ok := response["queryResult"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, requestId, resultPayload["requestId"])
	require.Equal(t, true, resultPayload["done"])

	testSvc.mu.Lock()
	defer testSvc.mu.Unlock()
	require.GreaterOrEqual(t, len(testSvc.received), 2)
	require.NotNil(t, testSvc.received[0].GetClientHello())
	require.NotNil(t, testSvc.received[1].GetExecuteQuery())
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	handler, err := New(Options{
		Endpoint: "127.0.0.1:9",
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memql/ws", nil)

	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestHandlerAllowsAnyAuthenticatedUser(t *testing.T) {
	// Any authenticated user should be allowed to connect (not just developers)
	// The handler should attempt WebSocket upgrade (426) not reject with 403
	handler, err := New(Options{
		Endpoint: "127.0.0.1:9",
	})
	require.NoError(t, err)

	claims := map[string]any{
		"role": "user",
		"sub":  "tester",
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memql/ws", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))

	handler.ServeHTTP(rr, req)

	// Should NOT be forbidden - any authenticated user can connect
	// The 426 (Upgrade Required) means the handler is trying to upgrade to WebSocket
	// which is the expected behavior for authenticated users
	require.NotEqual(t, http.StatusForbidden, rr.Code)
}

type stubMemqlService struct {
	memqlv1.UnimplementedMemqlServiceServer
	mu       sync.Mutex
	received []*memqlv1.MemqlClientMessage
	md       metadata.MD
}

func (s *stubMemqlService) Stream(stream memqlv1.MemqlService_StreamServer) error {
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		s.mu.Lock()
		s.md = md
		s.mu.Unlock()
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.received = append(s.received, msg)
		s.mu.Unlock()

		if exec := msg.GetExecuteQuery(); exec != nil {
			resp := &memqlv1.MemqlServerMessage{
				MessageId: uuid.NewString(),
				Payload: &memqlv1.MemqlServerMessage_QueryResult{
					QueryResult: &memqlv1.QueryResultChunk{
						RequestId: exec.GetRequestId(),
						Result:    &memqlv1.Result{},
						Done:      true,
					},
				},
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// TestMetadataFromRequest_CredentialPrecedence locks the WS bridge's
// credential-to-metadata mapping (#2511): Authorization header first, then
// the Sec-WebSocket-Protocol pair (bearer / guest), then the deprecated
// query params.
func TestMetadataFromRequest_CredentialPrecedence(t *testing.T) {
	get := func(md map[string][]string) string {
		if v := md["authorization"]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	// Header wins over everything.
	r := httptest.NewRequest(http.MethodGet, "/memql/ws?bearer_token=fromquery", nil)
	r.Header.Set("Authorization", "Bearer fromheader")
	r.Header.Set("Sec-WebSocket-Protocol", "bearer, fromsubprotocol")
	require.Equal(t, "Bearer fromheader", get(metadataFromRequest(r)))

	// Subprotocol bearer beats the query params.
	r = httptest.NewRequest(http.MethodGet, "/memql/ws?bearer_token=fromquery&token=legacy", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "bearer, fromsubprotocol")
	require.Equal(t, "Bearer fromsubprotocol", get(metadataFromRequest(r)))

	// Subprotocol guest maps to the Guest scheme.
	r = httptest.NewRequest(http.MethodGet, "/memql/ws", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "guest, invite-abc")
	require.Equal(t, "Guest invite-abc", get(metadataFromRequest(r)))

	// Deprecated query params still work when nothing else is present.
	r = httptest.NewRequest(http.MethodGet, "/memql/ws?bearer_token=fromquery", nil)
	require.Equal(t, "Bearer fromquery", get(metadataFromRequest(r)))
	r = httptest.NewRequest(http.MethodGet, "/memql/ws?token=legacy", nil)
	require.Equal(t, "Bearer legacy", get(metadataFromRequest(r)))
	r = httptest.NewRequest(http.MethodGet, "/memql/ws?guest_token=ginvite", nil)
	require.Equal(t, "Guest ginvite", get(metadataFromRequest(r)))
}

// TestHandlerNegotiatesBearerSubprotocol dials the bridge the way a browser
// does under #2511 -- new WebSocket(url, ["bearer", token]) -- and asserts
// (a) the 101 negotiates the "bearer" subprotocol back (browsers abort the
// handshake otherwise) and (b) the proxied gRPC stream carries the bearer in
// its authorization metadata.
func TestHandlerNegotiatesBearerSubprotocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	testSvc := &stubMemqlService{}
	memqlv1.RegisterMemqlServiceServer(grpcServer, testSvc)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := New(Options{Logger: logger, Endpoint: lis.Addr().String()})
	require.NoError(t, err)

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{"role": "user", "sub": "tester"}
		ctx := auth.ContextWithClaims(r.Context(), claims)
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer app.Close()

	wsURL := "ws" + strings.TrimPrefix(app.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL+"/memql/ws", &websocket.DialOptions{
		Subprotocols: []string{"bearer", "test-jwt-123"},
	})
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	require.Equal(t, "bearer", resp.Header.Get("Sec-WebSocket-Protocol"),
		"the 101 must echo the scheme subprotocol or browsers abort the handshake")
	require.Equal(t, "bearer", conn.Subprotocol())

	// The bridge opens the gRPC stream during the upgrade; wait for the
	// ClientHello to confirm the stream metadata arrived.
	require.Eventually(t, func() bool {
		testSvc.mu.Lock()
		defer testSvc.mu.Unlock()
		return len(testSvc.received) >= 1
	}, time.Second, 25*time.Millisecond, "memql service did not receive the hello")

	testSvc.mu.Lock()
	md := testSvc.md
	testSvc.mu.Unlock()
	require.NotNil(t, md)
	require.Equal(t, []string{"Bearer test-jwt-123"}, md.Get("authorization"))
}
