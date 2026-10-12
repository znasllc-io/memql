package memqlws

// A refusal the bridge writes itself, queued behind an answer that is still
// being written.
//
// The WebSocket library writes one message at a time, and a message waits for the
// writer's lock with the same context its write is limited by. A refusal had a
// deadline of its own, the base write timeout, while the answer ahead of it could
// hold the lock for the answer's own scaled deadline (42 s for 32 MiB). So the
// refusal gave up waiting at its deadline ("failed to acquire lock: context
// deadline exceeded"), the local error loop discarded it, and the browser's call,
// which has no timeout of its own, never settled. A message's deadline is for
// writing it, so it starts when it is the message's turn.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
	"nhooyr.io/websocket"
)

// Deterministic, at the session: an answer is being written to a peer that is not
// reading, a refusal is sent behind it under a write deadline of 200 ms, and the
// peer, once it reads, receives both, the answer first.
func TestARefusalQueuedBehindALongAnswerIsDelivered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverConn, peer := websocketPair(t, ctx)
	s := &session{ctx: ctx, conn: serverConn, writeLimit: 200 * time.Millisecond}

	const answerBytes = 16 << 20
	answered := make(chan error, 1)
	go func() { answered <- s.writeMessage(make([]byte, answerBytes)) }()
	require.Eventually(t, func() bool { return s.writing.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"the answer never began to be written")

	refused := make(chan error, 1)
	go func() {
		refused <- s.sendQueuedLocalError(&queuedLocalError{
			requestId: "r-1", correlate: "m-1", code: codes.ResourceExhausted, message: "too many concurrent MemQL queries",
		})
	}()

	// Three times the refusal's own deadline passes while the answer is still going.
	select {
	case err := <-refused:
		t.Fatalf("the refusal was given up on, or sent ahead of the answer, while the answer was still being written: %v", err)
	case <-time.After(600 * time.Millisecond):
	}

	_, answer, err := peer.Read(ctx)
	require.NoError(t, err)
	require.Len(t, answer, answerBytes)
	require.NoError(t, <-answered)

	_, data, err := peer.Read(ctx)
	require.NoError(t, err, "the refusal never reached the peer")
	require.NoError(t, <-refused)
	var frame map[string]any
	require.NoError(t, json.Unmarshal(data, &frame))
	require.Equal(t, "m-1", frame["correlateTo"])
	require.Equal(t, "r-1", frame["queryError"].(map[string]any)["requestId"])
}

// Through a bridge, the way the reviewer found it: a 16 MiB answer to a client
// that is slow to read, an oversized request refused while the answer is still
// going, a base write timeout of 200 ms. Both reach the client.
func TestARefusalBehindALongAnswerReachesASlowClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const answerBytes = 16 << 20
	large := strings.Repeat("a", answerBytes)
	svc := &stubMemqlService{answer: func(exec *memqlv1.ExecuteQueryMsg) *memqlv1.Result {
		if exec.GetQuery() == "large" {
			return &memqlv1.Result{Data: []*structpb.Value{structpb.NewStringValue(large)}}
		}
		return &memqlv1.Result{}
	}}
	conn := dialBridge(t, ctx, startBridge(t, Options{WriteTimeout: 200 * time.Millisecond, MaxMessageBytes: 64 << 10}, svc))

	answerID := sendQuery(t, ctx, conn, "large")
	time.Sleep(time.Second) // the answer is being written to a client that is not reading
	refusedID := sendQuery(t, ctx, conn, strings.Repeat("q", 100<<10))
	time.Sleep(700 * time.Millisecond) // well past the refusal's own deadline
	var answer, refusal map[string]any
	for i := 0; i < 2; i++ {
		readCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		var frame map[string]any
		err := wsReadJSON(readCtx, conn, &frame)
		stop()
		require.NoError(t, err, "frame %d: the client was sent one of the two and not the other", i+1)
		if _, isError := frame["queryError"]; isError {
			refusal = frame
		} else {
			answer = frame
		}
	}
	require.NotNil(t, answer, "the answer never reached the client")
	require.Equal(t, answerID, answer["queryResult"].(map[string]any)["requestId"])
	require.NotNil(t, refusal, "the refusal was dropped")
	require.Equal(t, refusedID, refusal["correlateTo"])
	require.Equal(t, refusedID, refusal["queryError"].(map[string]any)["requestId"])
}

func wsReadJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Waiting for a turn ends with the session. A writer queued behind a write that
// goes on does not outlive the session it belongs to.
func TestAWriterWaitingForItsTurnLeavesWhenTheSessionEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverConn, _ := websocketPair(t, ctx)
	sessionCtx, end := context.WithCancel(ctx)
	s := &session{ctx: sessionCtx, conn: serverConn, writeLimit: 30 * time.Second}

	// Somebody has the turn and keeps it, whatever becomes of the session.
	require.NoError(t, s.turn.acquire(context.Background()))
	defer s.turn.release()

	waiting := make(chan error, 1)
	go func() { waiting <- s.writeMessage([]byte("{}")) }()
	select {
	case err := <-waiting:
		t.Fatalf("a writer that had not had its turn returned: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	end()
	select {
	case err := <-waiting:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("a writer waiting for its turn outlived its session")
	}
}
