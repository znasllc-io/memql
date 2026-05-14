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
	"github.com/visionarys-io/memql/component/auth"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
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
}

func (s *stubMemqlService) Stream(stream memqlv1.MemqlService_StreamServer) error {
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
