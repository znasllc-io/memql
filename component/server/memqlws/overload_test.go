package memqlws

// What an overloaded bridge says to the request it refuses.
//
// The browser SDK finds the request a reply belongs to by one thing: the
// reply's correlateTo, which must be the message id the SDK assigned to the
// request (route() in sdk/ts/src/client/dispatcher.ts). A frame with no
// correlateTo is handed to the event listeners, and the request it was meant
// for stays pending until its caller gives up. The bridge's own refusal for a
// full queue ("too many concurrent MemQL queries") carried none, so a refused
// request never settled. The other half of this test is the SDK's, in
// sdk/ts/test/bridgeOverload.test.ts: a call that receives this frame settles.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/metrics"
	"nhooyr.io/websocket"
)

// sdkDispatcher plays the browser SDK's dispatcher over a WebSocket: sendAndWait
// gives a request a message id, writes the payload first and the id last (the
// order JSON.stringify gives the SDK's object), and keeps the request pending;
// every frame from the bridge settles the pending request its correlateTo names,
// and any other goes to the event listeners.
type sdkDispatcher struct {
	conn *websocket.Conn

	mu      sync.Mutex
	pending map[string]chan map[string]any // by message id
	events  []map[string]any
	frames  int
}

func newSDKDispatcher(ctx context.Context, conn *websocket.Conn) *sdkDispatcher {
	d := &sdkDispatcher{conn: conn, pending: map[string]chan map[string]any{}}
	go d.route(ctx)
	return d
}

// sendAndWait is a request that is pending until a reply names it. It returns the
// ids the request went out with and the channel its reply settles.
func (d *sdkDispatcher) sendAndWait(ctx context.Context, query string) (messageID, requestID string, settled <-chan map[string]any, err error) {
	messageID, requestID = uuid.NewString(), uuid.NewString()
	reply := make(chan map[string]any, 1)
	d.mu.Lock()
	d.pending[messageID] = reply
	d.mu.Unlock()
	text, err := json.Marshal(struct {
		ExecuteQuery map[string]any `json:"executeQuery"`
		MessageID    string         `json:"messageId"`
	}{map[string]any{"requestId": requestID, "query": query}, messageID})
	if err != nil {
		return "", "", nil, err
	}
	return messageID, requestID, reply, d.conn.Write(ctx, websocket.MessageText, text)
}

func (d *sdkDispatcher) route(ctx context.Context) {
	for {
		_, data, err := d.conn.Read(ctx)
		if err != nil {
			return
		}
		var frame map[string]any
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		d.mu.Lock()
		d.frames++
		correlateTo, _ := frame["correlateTo"].(string)
		if reply, waiting := d.pending[correlateTo]; correlateTo != "" && waiting {
			delete(d.pending, correlateTo)
			reply <- frame
		} else {
			d.events = append(d.events, frame)
		}
		d.mu.Unlock()
	}
}

func (d *sdkDispatcher) unsolicited() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.events...)
}

func (d *sdkDispatcher) stillPending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

func (d *sdkDispatcher) frameCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.frames
}

// A bridge whose queue is full answers the request it refuses by the id that
// request is waiting for, so the SDK's call settles with the refusal instead of
// hanging. The server is held busy and one request may be in flight, so the queue
// fills and the requests past it are refused; every request, refused or not, must
// settle.
func TestAnOverloadedBridgeAnswersTheRequestItRefused(t *testing.T) {
	before := metrics.WSQueryRejectedValue()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hold := make(chan struct{})
	svc := &stubMemqlService{hold: hold}
	conn := dialBridge(t, ctx, startBridge(t, Options{MaxConcurrentRequests: 1}, svc))
	sdk := newSDKDispatcher(ctx, conn)

	// One request in flight, one in the dispatcher's hand and defaultMaxPending
	// queued: 66 are taken, and the rest of 80 are refused.
	const requests = 80
	type sent struct {
		messageID, requestID string
		reply                <-chan map[string]any
	}
	var all []sent
	for i := 0; i < requests; i++ {
		messageID, requestID, reply, err := sdk.sendAndWait(ctx, "select")
		require.NoError(t, err)
		all = append(all, sent{messageID, requestID, reply})
	}
	settled := map[string]map[string]any{} // by message id
	collect := func() {
		for _, request := range all {
			if _, done := settled[request.messageID]; done {
				continue
			}
			select {
			case frame := <-request.reply:
				settled[request.messageID] = frame
			default:
			}
		}
	}

	// The refusals come while the server is still holding every answer. They are
	// paced, so give them time, then see where each went.
	require.Eventually(t, func() bool { return sdk.frameCount() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the bridge refused nothing: the queue never filled")
	time.Sleep(1500 * time.Millisecond)
	collect()
	if events := sdk.unsolicited(); len(events) > 0 {
		t.Fatalf("%d refusals named no request the SDK was waiting for and went to its event listeners, "+
			"so the calls they were meant for stay pending (first: %v)", len(events), events[0])
	}
	require.NotEmpty(t, settled, "the server was holding every answer, and no request was refused")
	for _, request := range all {
		frame, done := settled[request.messageID]
		if !done {
			continue
		}
		refusal, ok := frame["queryError"].(map[string]any)
		require.True(t, ok, "a request settled before the server answered with something that is not a refusal: %v", frame)
		require.Equal(t, request.requestID, refusal["requestId"])
		detail := refusal["error"].(map[string]any)
		require.Equal(t, "ResourceExhausted", detail["code"])
		require.Equal(t, "too many concurrent MemQL queries", detail["message"])
		require.Equal(t, request.messageID, frame["correlateTo"])
	}
	refusedWhileHeld := len(settled)
	require.GreaterOrEqual(t, metrics.WSQueryRejectedValue()-before, float64(refusedWhileHeld))

	// The server stops holding: everything that was taken is answered, and nothing
	// is left waiting.
	close(hold)
	require.Eventually(t, func() bool { collect(); return len(settled) == requests }, 15*time.Second, 20*time.Millisecond,
		"%d requests never settled", requests-len(settled))
	require.Zero(t, sdk.stillPending())
	require.Empty(t, sdk.unsolicited())
	require.Equal(t, requests-refusedWhileHeld, countResults(settled), "every request that was not refused was answered with a result")
}

func countResults(settled map[string]map[string]any) (n int) {
	for _, frame := range settled {
		if _, isResult := frame["queryResult"]; isResult {
			n++
		}
	}
	return n
}
