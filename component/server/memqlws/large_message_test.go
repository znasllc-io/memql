package memqlws

// Tests for what the bridge does with a message that is large in either
// direction.
//
// A request that is too large used to end the whole session: the read limit of
// the WebSocket library closed the connection, so one oversized request took every
// other request on the tab, and every live feed riding the socket, with it. And a
// large answer was written under the same fixed deadline as a small one, so a
// browser that was slow to take 20 MiB ended the session the same way. The
// contract these tests pin: a request is accepted up to the server's own message
// cap; a request over the limit fails by itself, with a sentence, and the session
// carries on; and the time allowed to write an answer grows with its size.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// startBridge serves the bridge in front of a real gRPC server running svc, with
// the server's own message caps, and returns the WebSocket URL of the bridge.
func startBridge(t *testing.T, opts Options, svc *stubMemqlService) string {
	t.Helper()
	return startBridgeWith(t, opts, svc, bridgeSetup{})
}

// bridgeSetup is how a test's bridge differs from the real one beyond its Options.
type bridgeSetup struct {
	// serverCap is the server's message cap, in both directions, and the cap the
	// bridge measures a request against (default: the real one, 32 MiB). A small
	// cap tries the measure without building requests of 32 MiB.
	serverCap int
	// anonymous is a session that has not signed in: the upgrade carries no
	// identity, as the auth middleware leaves a request it verified no credential
	// on. The bridge takes it only with public reads enabled.
	anonymous bool
}

func startBridgeWith(t *testing.T, opts Options, svc *stubMemqlService, setup bridgeSetup) string {
	t.Helper()
	serverCap := setup.serverCap
	if serverCap == 0 {
		serverCap = grpclimits.MaxMessageBytes
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer(
		grpc.MaxSendMsgSize(serverCap),
		grpc.MaxRecvMsgSize(serverCap),
	)
	memqlv1.RegisterMemqlServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	opts.Endpoint = lis.Addr().String()
	handler, err := New(opts)
	require.NoError(t, err)
	handler.serverMessageBytes = serverCap
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if setup.anonymous {
			handler.ServeHTTP(w, r)
			return
		}
		claims := map[string]any{"role": "admin", "sub": "tester"}
		handler.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
	}))
	t.Cleanup(app.Close)
	return "ws" + strings.TrimPrefix(app.URL, "http")
}

// dialBridge connects as a browser would. The test client's own read limit is
// lifted: the only limits in play are the bridge's.
func dialBridge(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url+"/memql/ws", nil)
	require.NoError(t, err)
	conn.SetReadLimit(64 << 20)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	return conn
}

func sendQuery(t *testing.T, ctx context.Context, conn *websocket.Conn, query string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{
		"messageId":    id,
		"executeQuery": map[string]any{"requestId": id, "query": query},
	}))
	return id
}

func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	var frame map[string]any
	require.NoError(t, wsjson.Read(ctx, conn, &frame))
	return frame
}

func receivedQueries(svc *stubMemqlService) []string {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	var queries []string
	for _, msg := range svc.received {
		if exec := msg.GetExecuteQuery(); exec != nil {
			queries = append(queries, exec.GetQuery())
		}
	}
	return queries
}

// The old frame limit was 5 MiB, which a template save does not need to outgrow
// to be refused: a document of up to 2 MiB travels as a JSON string, and escaping
// can inflate it several-fold. The bridge now takes a request up to the server's
// own message cap, so a request the server would accept is a request the bridge
// passes on.
func TestHandlerAcceptsARequestPastTheOldFrameLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))

	// 6 MiB: over the 5 MiB the bridge used to take, under the 32 MiB the server does.
	const queryBytes = 6 << 20
	id := sendQuery(t, ctx, conn, strings.Repeat("q", queryBytes))
	frame := readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "expected a queryResult for a request of %d bytes, got %v", queryBytes, frame)
	require.Equal(t, id, result["requestId"])
	queries := receivedQueries(svc)
	require.Len(t, queries, 1)
	require.Len(t, queries[0], queryBytes, "the server must receive the request whole")

	// And the session is still there.
	next := sendQuery(t, ctx, conn, "small")
	frame = readFrame(t, ctx, conn)
	result, ok = frame["queryResult"].(map[string]any)
	require.True(t, ok, "expected a queryResult, got %v", frame)
	require.Equal(t, next, result["requestId"])
}

// A request over the limit fails by itself. The sentence is the answer to THAT
// request, found by the ids it came with and carrying the id the sender waits for
// (correlateTo, which the browser SDK matches on), the server never sees it, and
// the next request on the same socket is answered as ever.
//
// Two senders are tried because they write the ids in different places. A Go
// client writing a map puts the keys in order, so both ids come after the query. The
// browser SDK writes the payload first, with the request id ahead of the query, and
// adds the message id last.
func TestHandlerRefusesAnOversizedRequestWithoutEndingTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	const limit = 64 << 10
	conn := dialBridge(t, ctx, startBridge(t, Options{MaxMessageBytes: limit}, svc))

	// 100 KiB: over the limit, under twice the limit.
	over := strings.Repeat("q", 100<<10)
	for _, c := range []struct {
		name string
		send func() (messageID, requestID string)
	}{
		{"ids after the query (a map in key order)", func() (string, string) {
			id := sendQuery(t, ctx, conn, over)
			return id, id
		}},
		{"the request id first and the message id last (the browser SDK)", func() (string, string) {
			messageID, requestID := uuid.NewString(), uuid.NewString()
			text := `{"executeQuery":{"requestId":"` + requestID + `","query":"` + over + `"},"messageId":"` + messageID + `"}`
			require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(text)))
			return messageID, requestID
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			messageID, requestID := c.send()
			frame := readFrame(t, ctx, conn)
			refusal, ok := frame["queryError"].(map[string]any)
			require.True(t, ok, "expected a queryError for a request over the limit, got keys of %v", len(frame))
			require.Equal(t, requestID, refusal["requestId"], "the refusal must name the request it refuses")
			require.Equal(t, messageID, frame["correlateTo"], "the refusal must carry the id the sender waits for")
			detail := refusal["error"].(map[string]any)
			require.Equal(t, "ResourceExhausted", detail["code"])
			require.Regexp(t, regexp.MustCompile(`^This request is too large to send: it is \d+ KiB and one request can carry at most 64 KiB\. Send less at once and try again\.$`),
				detail["message"])
			metadata := detail["metadata"].(map[string]any)
			require.Equal(t, "REQUEST_TOO_LARGE", metadata["code"])
			require.Equal(t, "65536", metadata["limitBytes"])
			for _, query := range receivedQueries(svc) {
				require.NotEqual(t, over, query, "the server must never see a request the bridge refused")
			}

			// The session carries on.
			next := sendQuery(t, ctx, conn, "small")
			frame = readFrame(t, ctx, conn)
			result, ok := frame["queryResult"].(map[string]any)
			require.True(t, ok, "the session did not survive the refusal: %v", frame)
			require.Equal(t, next, result["requestId"])
		})
	}
	require.Equal(t, []string{"small", "small"}, receivedQueries(svc))
}

// The bridge reads and discards what is over the limit, up to twice the limit.
// Past that it stops reading and closes the connection: a client that sends
// without end is not one request that failed.
func TestHandlerClosesTheConnectionOnlyForAMessageFarOverTheLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	const limit = 64 << 10
	conn := dialBridge(t, ctx, startBridge(t, Options{MaxMessageBytes: limit}, svc))

	sendQuery(t, ctx, conn, strings.Repeat("q", 3*limit))
	var frame map[string]any
	err := wsjson.Read(ctx, conn, &frame)
	require.Error(t, err, "a message of three times the limit was answered: %v", frame)
	require.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	require.Empty(t, receivedQueries(svc))
}

// The ids of an oversized request are looked for in the start of it and in the end
// of it, because the middle is never held. The browser SDK writes the request id
// first and adds the message id LAST, after the query, and the SDK finds the
// refusal by that message id (correlateTo), so the end matters as much as the
// start. They are found when the message is JSON text, whatever the order of its
// keys, and when it is protocol buffers.
func TestIDsOfAnOversizedRequestAreFoundInItsStartAndEnd(t *testing.T) {
	for _, c := range []struct {
		name                     string
		prefix                   string
		tail                     string
		wantMessage, wantRequest string
	}{
		{"both, in the usual order", `{"messageId":"m-1","executeQuery":{"requestId":"r-1","query":"select"}}`, ``, "m-1", "r-1"},
		{"the request first", `{"executeQuery":{"query":"select","requestId":"r-2"},"messageId":"m-2"}`, ``, "m-2", "r-2"},
		{"cut inside the query", `{"messageId":"m-3","executeQuery":{"requestId":"r-3","query":"sele`, ``, "m-3", "r-3"},
		{"cut before the request", `{"messageId":"m-4","executeQuery":{"requ`, ``, "m-4", ""},
		{"a message with no request id", `{"messageId":"m-5","subscribe":{"concept":"v1:x:y"}}`, ``, "m-5", ""},
		{"a request id one level deeper is not the request's", `{"messageId":"m-6","executeQuery":{"variables":{"requestId":"not-it"},"requestId":"r-6"}}`, ``, "m-6", "r-6"},
		{"numbers and nested values before the ids", `{"n":1,"b":true,"x":null,"list":[1,"requestId",{"requestId":"deep"}],"messageId":"m-7","executeQuery":{"requestId":"r-7"}}`, ``, "m-7", "r-7"},
		{"not JSON at all", `hello`, ``, "", ""},
		{"empty", ``, ``, "", ""},

		// What the browser SDK writes: the request id first in the payload, the
		// message id last, after a query that is never held.
		{"the SDK's order", `{"executeQuery":{"requestId":"r-8","query":"sele`, `ct 1"},"messageId":"m-8"}`, "m-8", "r-8"},
		// What a client writes that keeps its keys in order, as Go does for a map:
		// both ids after the query.
		{"both ids after the query", `{"executeQuery":{"query":"sele`, `ct 1","requestId":"r-10"},"messageId":"m-10"}`, "m-10", "r-10"},
		{"the last of several is the message's", `{"executeQuery":{"query":"sele`, `"messageId":"inside the query","x"},"requestId":"r-11"},"messageId":"m-11"}`, "m-11", "r-11"},
		{"nothing in the end either", `{"executeQuery":{"query":"sele`, `ct 1"}}`, "", ""},
		{"a prefix that has them wins over a tail that has others", `{"messageId":"m-12","executeQuery":{"requestId":"r-12","query":"x`, `"messageId":"other"}`, "m-12", "r-12"},
	} {
		t.Run(c.name, func(t *testing.T) {
			messageID, requestID := idsOfAnOversizedRequest(websocket.MessageText, []byte(c.prefix), []byte(c.tail))
			require.Equal(t, c.wantMessage, messageID)
			require.Equal(t, c.wantRequest, requestID)
		})
	}

	t.Run("protocol buffers", func(t *testing.T) {
		raw, err := proto.Marshal(&memqlv1.MemqlClientMessage{
			MessageId: "m-9",
			Payload:   &memqlv1.MemqlClientMessage_ExecuteQuery{ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: "r-9", Query: strings.Repeat("q", 1000)}},
		})
		require.NoError(t, err)

		// Cut inside the query, as the bridge's copy of an oversized message is.
		messageID, requestID := idsOfAnOversizedRequest(websocket.MessageBinary, raw[:300], nil)
		require.Equal(t, "m-9", messageID)
		require.Equal(t, "r-9", requestID)

		// Cut before the payload: the message id alone.
		messageID, requestID = idsOfAnOversizedRequest(websocket.MessageBinary, raw[:5], nil)
		require.Equal(t, "m-9", messageID)
		require.Equal(t, "", requestID)

		// Not protocol buffers at all.
		messageID, requestID = idsOfAnOversizedRequest(websocket.MessageBinary, []byte{0xff, 0xff, 0xff}, nil)
		require.Equal(t, "", messageID)
		require.Equal(t, "", requestID)

		// Correlate-to and metadata come before the payload and are not requests.
		with, err := proto.Marshal(&memqlv1.MemqlClientMessage{
			MessageId:   "m-13",
			CorrelateTo: "c-13",
			Metadata:    map[string]string{"k": "v"},
			Payload:     &memqlv1.MemqlClientMessage_ExecuteQuery{ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: "r-13", Query: "select"}},
		})
		require.NoError(t, err)
		messageID, requestID = idsOfAnOversizedRequest(websocket.MessageBinary, with, nil)
		require.Equal(t, "m-13", messageID)
		require.Equal(t, "r-13", requestID)
	})
}

// readRequest keeps the start and the end of a message that is over the limit and
// counts all of it, whether the end is longer than what the limit already held or
// shorter.
func TestReadRequestKeepsTheStartAndTheEndOfAnOversizedMessage(t *testing.T) {
	const limit = 100
	body := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + i%26)
		}
		return b
	}
	for _, n := range []int{limit, limit + 1, limit + 50, limit + idTailBytes - 1, limit + idTailBytes, limit + 10*idTailBytes + 7} {
		message := body(n)
		data, over, err := readRequest(bytes.NewReader(message), limit, nil)
		require.NoError(t, err)
		if n <= limit {
			require.Nil(t, over)
			require.Equal(t, message, data)
			continue
		}
		require.Nil(t, data)
		require.NotNil(t, over)
		require.EqualValues(t, n, over.size)
		require.Equal(t, message[:min(n, idPrefixBytes)][:min(limit+1, idPrefixBytes)], over.prefix[:min(limit+1, idPrefixBytes)], "the start of a message of %d bytes", n)
		want := message[max(0, n-idTailBytes):]
		require.Equal(t, want, over.tail, "the end of a message of %d bytes", n)
	}
}

func TestWriteDeadlineScalesWithTheMessage(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		size int
		want time.Duration
	}{
		{0, 10 * time.Second},
		{1 << 10, 10*time.Second + time.Second/1024},
		{mib / 2, 10*time.Second + 500*time.Millisecond},
		{mib, 11 * time.Second},
		{20 * mib, 30 * time.Second},
		{32 * mib, 42 * time.Second},
	} {
		require.Equal(t, c.want, writeDeadline(10*time.Second, c.size), "a message of %d bytes", c.size)
	}
}

// A browser on an ordinary connection takes a large answer over seconds, not
// within the time a small one is allowed. The write deadline used to be the same
// for both: the write of a 16 MiB answer to a client that was not reading yet ran
// into it, and the session ended. The deadline now grows with the answer.
func TestHandlerGivesALargeAnswerTimeToReachASlowClient(t *testing.T) {
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
	// A write deadline of 200 ms for a small message: a slow client is far past it.
	conn := dialBridge(t, ctx, startBridge(t, Options{WriteTimeout: 200 * time.Millisecond}, svc))

	id := sendQuery(t, ctx, conn, "large")
	time.Sleep(1500 * time.Millisecond) // the browser is not reading yet
	frame := readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "expected the large answer, got keys of %v", len(frame))
	require.Equal(t, id, result["requestId"])

	next := sendQuery(t, ctx, conn, "small")
	frame = readFrame(t, ctx, conn)
	result, ok = frame["queryResult"].(map[string]any)
	require.True(t, ok, "the session did not survive the slow client: %v", frame)
	require.Equal(t, next, result["requestId"])
}

// websocketPair is two ends of a WebSocket connection: the one a session runs on,
// and a peer that reads nothing unless the test makes it, so that it answers no
// ping (its library answers a ping only while a read is running).
func websocketPair(t *testing.T, ctx context.Context) (session, peer *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		accepted <- conn
		<-finished
	}))
	t.Cleanup(server.Close)
	// Registered after the server's, so it runs first and lets the handler return.
	t.Cleanup(func() { close(finished) })
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	client.SetReadLimit(64 << 20)
	// Not a graceful close: nothing reads on the other side to answer it.
	t.Cleanup(func() { _ = client.CloseNow() })
	return <-accepted, client
}

// A ping that cannot be answered while a message is moving is not a dead
// connection. A large answer is written as one frame, so the ping waits behind it
// for the write lock, and a browser sending a large request sends it as one frame
// that its pong cannot interleave with. Two missed pings ended the session, so a
// transfer the deadline would have allowed was cut by the keepalive.
//
// So no ping is sent while a message is being written (it ends in its own
// deadline or error), nor while one is arriving and bytes of it are still coming.
// A message that has started to arrive and then stalls is not moving: the
// keepalive is what notices a sender that has gone, and it is not switched off for
// a sender that has only stopped.
func TestNoPingIsSentWhileAMessageIsMoving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// pinger runs a ping loop on a connection whose peer answers no ping.
	pinger := func(t *testing.T, mutate func(s *session)) (*session, chan error) {
		serverConn, _ := websocketPair(t, ctx)
		sessionCtx, stop := context.WithCancel(ctx)
		t.Cleanup(stop)
		s := &session{ctx: sessionCtx, conn: serverConn, pingInterval: 20 * time.Millisecond, pingTimeout: 60 * time.Millisecond}
		mutate(s)
		ended := make(chan error, 1)
		go func() { ended <- s.pingLoop() }()
		return s, ended
	}
	requireEndsWithPingFailure := func(t *testing.T, ended chan error, why string) {
		t.Helper()
		select {
		case err := <-ended:
			require.ErrorContains(t, err, "ping failed")
		case <-time.After(5 * time.Second):
			t.Fatal(why)
		}
	}
	requireStillPinging := func(t *testing.T, ended chan error, for_ time.Duration, why string) {
		t.Helper()
		select {
		case err := <-ended:
			t.Fatalf("%s: %v", why, err)
		case <-time.After(for_):
		}
	}

	t.Run("with nothing moving, a peer that answers no ping ends the session", func(t *testing.T) {
		_, ended := pinger(t, func(*session) {})
		requireEndsWithPingFailure(t, ended, "a peer that answers no ping was never given up on")
	})

	t.Run("while a message is being written it does not, and once it is written it does", func(t *testing.T) {
		s, ended := pinger(t, func(s *session) { s.writing.Add(1) })
		requireStillPinging(t, ended, 600*time.Millisecond, "the keepalive ended the session during a write")
		s.writing.Add(-1)
		requireEndsWithPingFailure(t, ended, "pings did not resume once the write was over")
	})

	t.Run("while a message is arriving and bytes of it are still coming it does not", func(t *testing.T) {
		s, ended := pinger(t, func(s *session) { s.reading.Add(1) })
		// Bytes arrive every 20 ms for 600 ms, as a slow upload's do.
		for end := time.Now().Add(600 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			s.noteRead()
			select {
			case err := <-ended:
				t.Fatalf("the keepalive ended the session during an upload that was still moving: %v", err)
			default:
			}
		}
		s.reading.Add(-1)
		requireEndsWithPingFailure(t, ended, "pings did not resume once the message had arrived")
	})

	t.Run("a message that started to arrive and stalled does not keep the session alive", func(t *testing.T) {
		s, ended := pinger(t, func(s *session) { s.reading.Add(1) })
		s.noteRead() // the first bytes, and then nothing
		requireEndsWithPingFailure(t, ended, "a sender that stopped mid-message held the session open")
	})

	// What keeps a message alive is the SENDER's bytes. The bridge's own writes to a
	// peer that has stopped sending (live-feed events, answers) are not the peer's
	// progress, and a peer that died mid-message on a socket still receiving them
	// would otherwise never be pinged and linger until TCP gave up.
	t.Run("the bridge's own writes do not keep a stalled message alive", func(t *testing.T) {
		s, ended := pinger(t, func(s *session) {
			s.reading.Add(1)
			s.writeLimit = 5 * time.Second
		})
		s.noteRead() // the first bytes, and then nothing from the peer
		go func() {
			// Live-feed events written to the peer every 20 ms, inside the window.
			for ticker := time.NewTicker(20 * time.Millisecond); ; <-ticker.C {
				if ctx.Err() != nil || s.ctx.Err() != nil {
					return
				}
				_ = s.writeMessage([]byte("{}"))
			}
		}()
		requireEndsWithPingFailure(t, ended, "a sender that stopped mid-message was kept alive by what the bridge wrote to it")
	})
}

// A message being written is a message moving, for as long as it takes the peer
// to take it. A write that waits for a peer that is not reading yet is the case:
// it holds the connection's write lock, which is what a ping would queue behind.
func TestAMessageBeingWrittenCountsAsMoving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverConn, peer := websocketPair(t, ctx)
	s := &session{ctx: ctx, conn: serverConn, writeLimit: 30 * time.Second}

	done := make(chan error, 1)
	go func() { done <- s.writeMessage(make([]byte, 16<<20)) }()
	require.Eventually(t, func() bool { return s.writing.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"a message being written was not counted as moving")
	select {
	case err := <-done:
		t.Fatalf("the write finished before the peer read anything: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	require.EqualValues(t, 1, s.writing.Load())

	_, data, err := peer.Read(ctx)
	require.NoError(t, err)
	require.Len(t, data, 16<<20)
	require.NoError(t, <-done)
	require.EqualValues(t, 0, s.writing.Load(), "a message that has been written is not moving")
}

// The keepalive does not cut a request that is still being sent, against a real
// peer that answers no ping because it is busy sending: a ping every 20 ms that is
// given 60 ms to be answered would end the session at the first miss. (A large
// answer waiting for a browser is the other direction. It cannot be tried
// against a peer that answers no ping the whole time, because before the answer
// is written a real browser would answer; it is pinned by the two tests above:
// a message being written counts as moving, and nothing is pinged while one is.)
func TestTheKeepaliveDoesNotCutARequestThatIsStillBeingSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	// Pings are due every 200 ms and given 60 ms; the pieces come every 150 ms, inside
	// the 400 ms window in which a message that is still arriving counts as moving.
	url := startBridge(t, Options{PingInterval: 200 * time.Millisecond, PingTimeout: 60 * time.Millisecond}, svc)

	conn := dialBridge(t, ctx, url)
	id := uuid.NewString()
	writer, err := conn.Writer(ctx, websocket.MessageText)
	require.NoError(t, err)
	// The message is sent in pieces over some time, and this client reads nothing
	// meanwhile, so it answers no ping. The first piece is large
	// enough to be sent at once: the library holds a small write back until more
	// comes, and a message that has not started arriving is not moving.
	pieces := []string{
		`{"messageId":"` + id + `","executeQuery":{"requestId":"` + id + `","query":"` + strings.Repeat("q", 20<<10),
		strings.Repeat("q", 20<<10),
		strings.Repeat("q", 20<<10),
		strings.Repeat("q", 20<<10),
		strings.Repeat("q", 20<<10),
		strings.Repeat("q", 20<<10),
		`"}}`,
	}
	for i, piece := range pieces {
		_, err := writer.Write([]byte(piece))
		require.NoError(t, err, "piece %d of the request", i)
		time.Sleep(150 * time.Millisecond)
	}
	require.NoError(t, writer.Close())
	frame := readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "the keepalive cut a request on its way in: got %v", frame)
	require.Equal(t, id, result["requestId"])
}
