//go:build agent

package worker

// THE APP-CALL HOP OVER A REAL NodeService CONNECTION.
//
// app_forward_hop_test.go joins the two nodes with an in-process link, which
// proves what crosses and who decides -- but its link stands in for the three
// pieces a production call actually rides: the planner's peerManagerSender over
// a real node.PeerManager, the WorkerDialer's stream to the agent, and the
// agent's NodeService receive loop (which registers each forwarded call before
// running it, so its cancel always finds it). Every test here goes through all
// three, over loopback gRPC. Only the machine's harness is a stand-in.
//
// TO CONFIRM THEY ARE LOAD-BEARING: revert peerManagerSender to the general
// outbox (the first test's "reported as sent" checks fail), or hand forwarded
// calls stream.Context() in component/node/stream_handler.go instead of the
// registered context (the cancel test leaves sessions running).

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// hookSender is the production sender with a hook run the moment a request
// has been handed to the stream -- how a test cancels a caller "right behind"
// its request without a sleep.
type hookSender struct {
	peerManagerSender
	mu    sync.Mutex
	after func()
}

func (s *hookSender) setAfter(fn func()) {
	s.mu.Lock()
	s.after = fn
	s.mu.Unlock()
}

func (s *hookSender) SendRequest(nodeId string, msg *nodev1.NodeClientMessage) (<-chan struct{}, error) {
	done, err := s.peerManagerSender.SendRequest(nodeId, msg)
	s.mu.Lock()
	after := s.after
	s.mu.Unlock()
	if err == nil && after != nil && msg.GetAppCallForwardRequest() != nil {
		after()
	}
	return done, err
}

// transportHop is the holding agent serving NodeService on loopback and a
// planner dialing it.
type transportHop struct {
	*appHop
	plannerPeers *node.PeerManager
	sender       *hookSender
	holderServer *node.NodeServer
}

// lateStartHandler is the holder's real forward handler, reached late: every
// forwarded app call waits before the handler sees it, the way a goroutine
// that has not been scheduled yet waits, while cancels go straight through.
// It is the window the receive loop's registration closes, held open long
// enough to be hit every time instead of now and then.
type lateStartHandler struct {
	*ForwardHandler
	late time.Duration
}

func (l lateStartHandler) HandleForwardedAppCall(ctx context.Context, req *nodev1.AppCallForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	time.Sleep(l.late)
	l.ForwardHandler.HandleForwardedAppCall(ctx, req, send)
}

func newTransportHop(t *testing.T, wrap ...func(*ForwardHandler) node.WorkerForwardHandler) *transportHop {
	t.Helper()
	h := newAppHop(t)
	logger := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// THE HOLDING AGENT: its real NodeServer, with the real forward handler
	// (and behind it the real app door) installed exactly as the agent's
	// cluster wiring installs it.
	addr := freeLoopbackAddress(t)
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", addr)
	holderIdentity := &node.Identity{ID: hopHolder, Type: node.NodeTypeAgent, Address: addr}
	holderPeers := node.NewPeerManager(holderIdentity, logger)
	server := node.NewNodeServer(holderIdentity, holderPeers, logger)
	var handler node.WorkerForwardHandler = h.link.handler
	if len(wrap) > 0 && wrap[0] != nil {
		handler = wrap[0](h.link.handler)
	}
	server.SetWorkerForwardHandler(handler)
	holderPeers.Start(ctx)
	server.Start(ctx)
	t.Cleanup(func() {
		server.Stop(context.Background())
		holderPeers.Stop(context.Background())
	})
	select {
	case <-server.Ready():
	case <-time.After(5 * time.Second):
		t.Fatalf("the holder's NodeService never came up on %s", addr)
	}

	// THE PLANNER: a PeerManager, a dialer to the agent, and the production
	// sender -- wrapped only to let a test act the moment a request leaves.
	plannerIdentity := &node.Identity{ID: hopPlanner, Type: node.NodeTypePlanner}
	plannerPeers := node.NewPeerManager(plannerIdentity, logger)
	plannerPeers.Start(ctx)
	sender := &hookSender{peerManagerSender: peerManagerSender{peerMgr: plannerPeers}}
	forward := newForwardRouter(sender, func() (string, string) { return hopPlanner, "planner" }, logger)
	dialer := node.NewWorkerDialer(plannerIdentity, plannerPeers, nil, nil,
		[]node.WorkerTarget{{NodeType: node.NodeTypeAgent, Address: addr}}, logger)
	dialer.SetWorkerForwardResponseSink(forward)
	dialer.Start(ctx)
	t.Cleanup(func() {
		dialer.Stop(context.Background())
		plannerPeers.Stop(context.Background())
	})

	// Linked when a request can be handed to a live stream.
	waitFor(t, "the planner's stream to the holding agent", func() bool {
		_, err := plannerPeers.SendRequest(hopHolder, heartbeatEnvelope())
		return err == nil
	})

	h.remote = NewRemoteAppInference(h.senderStore, nil, forward, hopPlanner, logger)
	h.remote.clock = fleetNow
	return &transportHop{appHop: h, plannerPeers: plannerPeers, sender: sender, holderServer: server}
}

func heartbeatEnvelope() *nodev1.NodeClientMessage {
	return &nodev1.NodeClientMessage{MessageId: "probe",
		Payload: &nodev1.NodeClientMessage_Heartbeat{Heartbeat: &nodev1.NodeHeartbeat{}}}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// cancelControls counts the cancels the machine was sent: one per session the
// holder stopped.
func (m *machineHarness) cancelControls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.controls {
		if c.GetAction() == workerservice.AppSessionActionCancel {
			n++
		}
	}
	return n
}

// The planner's call crosses a real NodeService connection and is served by
// Claude Code on the laptop.
func TestAPlannerAppCallCrossesARealNodeServiceConnection(t *testing.T) {
	h := newTransportHop(t)
	ctx, cancel := plannerCtx(t)
	defer cancel()

	res, err := h.remote.Call(ctx, appRequest("classify: over the wire"))
	if err != nil {
		t.Fatalf("the planner's app call over a real connection failed: %v", err)
	}
	if res.Content != `{"complexity":"trivial"}` {
		t.Fatalf("answer = %q, want Claude Code's structured result", res.Content)
	}
	if sessions, actors := h.harness.sessions(); len(sessions) != 1 || actors[0] != hopOwner {
		t.Fatalf("sessions = %d actors = %v, want one session run as the owner", len(sessions), actors)
	}
}

// A CANCEL RIGHT BEHIND ITS REQUEST STOPS IT, over the real transport: the
// caller gives up the moment the request is on the stream, and the laptop must
// be left with no session running -- either none opened, or the one that did
// was cancelled.
//
// The holder's handler starts late (lateStartHandler), so the cancel is always
// read before the handler has registered anything of its own. That is the
// reviewer's reproduction -- a cancel read right after the handler was
// spawned lost 200 of 200 -- and it must lose none.
func TestACancelRightBehindItsRequestLeavesNoSessionRunningOnTheLaptop(t *testing.T) {
	h := newTransportHop(t, func(inner *ForwardHandler) node.WorkerForwardHandler {
		return lateStartHandler{ForwardHandler: inner, late: 50 * time.Millisecond}
	})
	h.harness.hang = true

	for i := 0; i < 5; i++ {
		ctx, cancel := plannerCtx(t)
		h.sender.setAfter(cancel)
		_, err := h.remote.Call(ctx, appRequest("classify: never mind"))
		h.sender.setAfter(nil)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: err = %v, want the caller's cancellation", i, err)
		}
		waitFor(t, "every session the holder opened to be cancelled on the laptop", func() bool {
			sessions, _ := h.harness.sessions()
			return len(sessions) == h.harness.cancelControls()
		})
	}
	// Settle past the late start, then look again: a session that opened after
	// the loop's last check would otherwise go unseen.
	time.Sleep(100 * time.Millisecond)
	sessions, _ := h.harness.sessions()
	if cancelled := h.harness.cancelControls(); len(sessions) != cancelled {
		t.Fatalf("%d session(s) opened on the laptop and %d were cancelled: a caller that gave up left Claude Code running",
			len(sessions), cancelled)
	}
}

// THE PRODUCTION SENDER REPORTS WHAT REALLY HAPPENED (the brief's "a failed
// send reported as failed"), over a real PeerManager: a peer it cannot reach
// is a refusal, a live stream carries the request and says when it ends, and
// a connection between attempts refuses a request but still queues a cancel.
func TestThePeerManagerSenderReportsWhatReallyHappened(t *testing.T) {
	h := newTransportHop(t)
	sender := peerManagerSender{peerMgr: h.plannerPeers}

	if _, err := sender.SendRequest("agent-nowhere", heartbeatEnvelope()); err == nil {
		t.Fatal("a request to a node the planner never heard of was reported as sent")
	}
	if sender.Send("agent-nowhere", heartbeatEnvelope()) {
		t.Fatal("Send reported a request to an unknown node as sent")
	}
	h.plannerPeers.RegisterMonitored(&nodev1.PeerInfo{NodeId: "agent-known", NodeType: string(node.NodeTypeAgent)})
	if _, err := sender.SendRequest("agent-known", heartbeatEnvelope()); err == nil {
		t.Fatal("a request to a peer the planner never dialed was reported as sent")
	}

	ended, err := sender.SendRequest(hopHolder, heartbeatEnvelope())
	if err != nil {
		t.Fatalf("a request to the holding agent's live stream was refused: %v", err)
	}
	select {
	case <-ended:
		t.Fatal("the stream was reported ended while it was live")
	default:
	}

	// The holder goes away: the stream the request left on ends, and the
	// sender is told.
	h.holderServer.Stop(context.Background())
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder's stream ended and the sender was never told -- an app call would wait out 10m30s")
	}
	waitFor(t, "a request to the vanished holder to be refused", func() bool {
		_, err := sender.SendRequest(hopHolder, heartbeatEnvelope())
		return err != nil
	})
	if sender.Send(hopHolder, heartbeatEnvelope()) {
		t.Fatal("Send reported a request as sent with no live stream to the holder")
	}
	// But a cancel is queued for the next attempt: a pull or a probe outlives
	// the stream that asked for it.
	if !sender.SendCancel(hopHolder, &nodev1.NodeClientMessage{MessageId: "c",
		Payload: &nodev1.NodeClientMessage_ModelPullForwardCancel{ModelPullForwardCancel: &nodev1.ModelPullForwardCancel{RequestId: "r"}}}) {
		t.Fatal("a cancel posted while the connection was between attempts was dropped")
	}

	// And the app door built on this sender fails over at once rather than
	// parking: the route's next Source is what a caller gets.
	ctx, cancel := plannerCtx(t)
	defer cancel()
	_, err = h.remote.Call(ctx, appRequest("classify: holder gone"))
	var refusal *memqlengine.AppUnavailable
	if !errors.As(err, &refusal) || ctx.Err() != nil {
		t.Fatalf("err = %v, want an immediate typed refusal naming the unreachable holder", err)
	}
}
