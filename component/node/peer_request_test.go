package node

// peer_request_test.go -- a request that could not be handed to a live stream
// is reported as NOT SENT.
//
// The worker forwards (tool dispatch, model call, app call) used to send on a
// peer's general outbox and report "handed off" regardless: Send drops a
// message when the outbox is full or the connection closed, and says so only
// in a log line. A dropped request then parked its sender until the caller's
// own deadline -- ten minutes for an app call -- for a reply that could never
// come, where the honest answer ("that replica is not reachable") would have
// let the route move on to its next source at once.

import (
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func requestEnvelope() *nodev1.NodeClientMessage {
	return &nodev1.NodeClientMessage{MessageId: "m", Payload: &nodev1.NodeClientMessage_AppCallForwardRequest{
		AppCallForwardRequest: &nodev1.AppCallForwardRequest{RequestId: "r"}}}
}

func TestSendRequestRefusesAPeerItCannotReachNow(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	if _, err := pm.SendRequest("agent-gone", requestEnvelope()); err == nil {
		t.Fatal("a request to a node this one has never heard of was reported as sent")
	}

	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: "agent-known", NodeType: string(NodeTypeAgent)})
	if _, err := pm.SendRequest("agent-known", requestEnvelope()); err == nil {
		t.Fatal("a request to a peer this node never dialed was reported as sent")
	}

	// Dialed, but no stream attempt is live: the connection is backing off
	// between attempts, or was never started. The general outbox would have
	// queued this for a future reconnect and called it sent.
	conn := newPeerConnection(testIdentity(), "agent-known", "127.0.0.1:1", testLogger())
	pm.AttachConnection("agent-known", conn)
	if _, err := pm.SendRequest("agent-known", requestEnvelope()); err == nil {
		t.Fatal("a request on a connection with no live stream was reported as sent")
	}

	// Closed.
	conn.Close()
	if _, err := pm.SendRequest("agent-known", requestEnvelope()); err == nil {
		t.Fatal("a request on a closed connection was reported as sent")
	}
}

func TestSendRequestHandsTheRequestToTheLiveStreamAndSaysWhenItEnds(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: "agent-live", NodeType: string(NodeTypeAgent)})
	conn := newPeerConnection(testIdentity(), "agent-live", "127.0.0.1:1", testLogger())
	attempt := &peerStream{done: make(chan struct{}), sendCh: make(chan *nodev1.NodeClientMessage, 1)}
	conn.mu.Lock()
	conn.current = attempt
	conn.mu.Unlock()
	pm.AttachConnection("agent-live", conn)

	ended, err := pm.SendRequest("agent-live", requestEnvelope())
	if err != nil {
		t.Fatalf("a request to a peer with a live stream was refused: %v", err)
	}
	select {
	case got := <-attempt.sendCh:
		if got.GetAppCallForwardRequest().GetRequestId() != "r" {
			t.Fatalf("the live stream carried %+v", got)
		}
	default:
		t.Fatal("the request was reported as sent and is not on the live stream")
	}

	// A full outbox is a refusal, not a silent drop.
	attempt.sendCh <- requestEnvelope()
	if _, err := pm.SendRequest("agent-live", requestEnvelope()); err == nil {
		t.Fatal("a request the live stream had no room for was reported as sent")
	}

	// The stream the request left on ending is how its sender learns the
	// holder went away mid-call, instead of waiting out its own deadline.
	select {
	case <-ended:
		t.Fatal("the stream was reported ended while it was live")
	default:
	}
	conn.mu.Lock()
	conn.endStreamLocked()
	conn.mu.Unlock()
	select {
	case <-ended:
	default:
		t.Fatal("the stream the request went out on ended and its sender was not told")
	}
}
