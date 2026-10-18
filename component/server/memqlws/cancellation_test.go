package memqlws

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"nhooyr.io/websocket/wsjson"
)

// The backend sees cancellation promptly, but is deliberately slow to finish
// stopping. Admission must count actual unfinished work across the WS/gRPC hop.
type cancellingService struct {
	memqlv1.UnimplementedMemqlServiceServer
	received chan *memqlv1.MemqlClientMessage
	finish   chan struct{}
}

func (s *cancellingService) Stream(stream memqlv1.MemqlService_StreamServer) error {
	var sendMu sync.Mutex
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if msg.GetExecuteQuery() != nil || msg.GetCancelRequest() != nil {
			s.received <- msg
		}
		if query := msg.GetExecuteQuery(); query != nil {
			go func() {
				select {
				case <-s.finish:
				case <-stream.Context().Done():
					return
				}
				sendMu.Lock()
				defer sendMu.Unlock()
				_ = stream.Send(&memqlv1.MemqlServerMessage{CorrelateTo: msg.GetMessageId(), Payload: &memqlv1.MemqlServerMessage_QueryResult{QueryResult: &memqlv1.QueryResultChunk{RequestId: query.GetRequestId(), Done: true}}})
			}()
		}
	}
}
func receiveQueryFrame(t *testing.T, ctx context.Context, svc *cancellingService) *memqlv1.MemqlClientMessage {
	t.Helper()
	select {
	case msg := <-svc.received:
		return msg
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}
func TestCancellationKeepsAdmissionUntilBackendStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc := &cancellingService{received: make(chan *memqlv1.MemqlClientMessage, 8), finish: make(chan struct{})}
	conn := dialBridge(t, ctx, startBridge(t, Options{MaxConcurrentRequests: 1}, svc))
	sdk := newSDKDispatcher(ctx, conn)
	_, first, firstReply, err := sdk.sendAndWait(ctx, "first()")
	require.NoError(t, err)
	require.Equal(t, first, receiveQueryFrame(t, ctx, svc).GetExecuteQuery().GetRequestId())
	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{"messageId": "cancel-first", "cancelRequest": map[string]any{"requestId": first}}))
	require.Equal(t, first, receiveQueryFrame(t, ctx, svc).GetCancelRequest().GetRequestId())
	_, second, secondReply, err := sdk.sendAndWait(ctx, "second()")
	require.NoError(t, err)
	select {
	case msg := <-svc.received:
		t.Fatalf("admitted replacement while cancelled work still runs: %s", msg.GetExecuteQuery().GetRequestId())
	case <-time.After(100 * time.Millisecond):
	}
	close(svc.finish)
	require.Equal(t, second, receiveQueryFrame(t, ctx, svc).GetExecuteQuery().GetRequestId())
	select {
	case <-firstReply:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-secondReply:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func TestQueuedCancellationAnswersOriginalRequestWithoutExecutingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc := &cancellingService{received: make(chan *memqlv1.MemqlClientMessage, 8), finish: make(chan struct{})}
	conn := dialBridge(t, ctx, startBridge(t, Options{MaxConcurrentRequests: 1}, svc))
	sdk := newSDKDispatcher(ctx, conn)
	_, _, _, err := sdk.sendAndWait(ctx, "hold()")
	require.NoError(t, err)
	receiveQueryFrame(t, ctx, svc)
	messageID, requestID, reply, err := sdk.sendAndWait(ctx, "neverRun()")
	require.NoError(t, err)
	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{"messageId": "cancel-queued", "cancelRequest": map[string]any{"requestId": requestID}}))
	require.Equal(t, requestID, receiveQueryFrame(t, ctx, svc).GetCancelRequest().GetRequestId())
	close(svc.finish)
	select {
	case frame := <-reply:
		require.Equal(t, messageID, frame["correlateTo"])
		require.Equal(t, "Canceled", frame["queryError"].(map[string]any)["error"].(map[string]any)["code"])
	case <-time.After(time.Second):
		t.Fatal("cancelled queued request never settled")
	}
	select {
	case msg := <-svc.received:
		t.Fatalf("cancelled queued query executed: %v", msg)
	default:
	}
}
