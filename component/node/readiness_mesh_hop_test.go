package node

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// EVERY READINESS PARTICIPANT CONVERGES, WHATEVER THE MESH DELIVERS
// (memql#5259).
//
// After a machine was paired on 2026-09-09, seven of fourteen `ai` rows
// recomputed and seven kept their boot answer: the other shared bff, both
// product bffs, both edges and both identity nodes. This reproduces that
// through the REAL source-to-subscriber path -- one events.Bus, PeerManager
// and EventBridge per replica, wired in the cloud's dial topology, with the
// production recompute loop (retry, safety net, debounce) subscribed to every
// replica's bus -- and asserts the thing the issue requires: every
// participant's row recomputes after the change, without a restart, including
// the ones the event never reaches.
//
// ===========================================================================
// WHY THE EVENT DOES NOT REACH EVERY NODE (the delivery evidence)
// ===========================================================================
// A forward goes only where a node holds an OUTBOUND connection
// (PeerManager.sendTarget), and a relay goes at most three hops along the
// receiver's own outbound dials; nothing pushes server->client. So:
//
//   - identity is excluded from every broadcast (meshEventParticipants), by
//     design, even though every bff dials it -- and the fix must not widen
//     that, because it would fire identity's subscribers on traffic it has
//     never seen;
//   - a node nobody dials -- the edge, a product bff -- hears nothing;
//   - the other shared bff hears nothing when every child's parent dial
//     landed on the first one (bff-active is a Service, so which pod a child
//     reaches is whichever round-robin gave it);
//   - a peer whose outbound connection is ABSENT, or DETACHED while
//     reconnecting, is skipped, and nothing queues the event for it.
//
// Those are the seven. The recompute loop does not need them to hear: the
// node that WROTE the registration hears its own event on its local bus, the
// fold sets aside a row whose cluster-scoped lanes lag the freshest one, and
// every other node's safety-net pass converges its row within one period.
func TestEveryReadinessParticipantConvergesWhateverTheMeshDelivers(t *testing.T) {
	m := newReadinessMesh(t)

	// The cloud's dial topology, as memql#5316's analysis measured it. Every
	// PARENT dial landed on bff-a -- the arrangement that left bff-b stale.
	m.dial("agent-a", "bff-a", "workbench-a")
	m.dial("agent-b", "bff-a", "workbench-a")
	m.dial("planner-a", "bff-a", "agent-a", "agent-b")
	m.dial("workbench-a", "bff-a")
	m.dial("bff-a", "identity-a", "workbench-a", "agent-a", "agent-b", "planner-a")
	m.dial("bff-b", "identity-a", "workbench-a", "agent-a", "agent-b", "planner-a")
	m.dial("bff-product-a", "identity-a", "workbench-a", "agent-a", "agent-b", "planner-a")
	m.dial("edge-a", "bff-a")

	// AN ABSENT OUTBOUND CONNECTION: bff-a knows planner-a but holds no
	// transport to it at the moment of the change, so the relay skips it.
	m.absent("bff-a", "planner-a")
	// A RECONNECTING ONE: bff-a's transport to agent-b is torn down across the
	// change and restored after it.
	m.detach("bff-a", "agent-b")
	// A FAILED READINESS WRITE: the edge's first pass after the change is
	// refused, the way a saturated database refused it.
	m.failNextWrite("edge-a")

	m.start()

	// THE PAIRING. The agent holding the machine's stream writes the
	// registration: the event lands on ITS bus, and its bridge forwards it.
	m.change(1, "agent-a")
	time.Sleep(50 * time.Millisecond)
	m.attach("bff-a", "agent-b")

	m.awaitConvergence(t, 1)

	// THE WRITER ALWAYS HEARS ITS OWN EVENT, which is what lets the fold
	// decide the verdict at once from the freshest row.
	if m.heard("agent-a") == 0 {
		t.Fatal("the node that wrote the registration did not hear its own event on its local bus")
	}
	// IDENTITY NEVER HEARS A BROADCAST -- the exclusion the fix must not
	// widen -- and converged anyway.
	if n := m.heard("identity-a"); n != 0 {
		t.Fatalf("identity heard %d broadcast registration event(s); meshEventParticipants must keep it out of every broadcast", n)
	}
	// The absent and the reconnecting transports lost the event: nothing
	// buffers a mesh forward for a peer that is not connected.
	for _, id := range []string{"planner-a", "agent-b"} {
		if n := m.heard(id); n != 0 {
			t.Fatalf("%s heard %d event(s) across a transport that was not there", id, n)
		}
	}
	// The failed write was retried rather than abandoned.
	if m.failures("edge-a") == 0 {
		t.Fatal("negative control failed: the edge's write was never refused, so the retry proved nothing")
	}

	// THE MESH ISLAND, as it stands when memql#5338 was filed. This is the
	// delivery evidence for the five replicas memql#5259 could not explain
	// with the identity filter; server-to-client push (memql#5340) is the
	// change that should turn it over, and that change updates this list --
	// the convergence above must keep passing without an edit.
	var island []string
	for _, id := range m.order {
		if m.heard(id) == 0 {
			island = append(island, id)
		}
	}
	sort.Strings(island)
	want := []string{"agent-b", "bff-b", "bff-product-a", "edge-a", "identity-a", "planner-a"}
	if len(island) != len(want) {
		t.Fatalf("the nodes the event did not reach are %v, want %v", island, want)
	}
	for i := range want {
		if island[i] != want[i] {
			t.Fatalf("the nodes the event did not reach are %v, want %v", island, want)
		}
	}
	t.Logf("delivery: %v", m.deliveries())

	// THE REVOCATION, again written on the agent and again converging
	// everywhere -- the second fact must not be lost behind the first.
	m.change(2, "agent-a")
	m.awaitConvergence(t, 2)
}

// ---------------------------------------------------------------------------
// The in-process mesh
// ---------------------------------------------------------------------------

type readinessReplica struct {
	id     string
	bus    *events.Bus
	pm     *PeerManager
	bridge *EventBridge
	heard  atomic.Int32

	mu          sync.Mutex
	failNext    bool
	failedTimes int
	seen        int64
}

type readinessMesh struct {
	t        *testing.T
	ctx      context.Context
	nodes    map[string]*readinessReplica
	order    []string
	conns    map[[2]string]*peerConnection
	absentTo map[[2]string]bool
	version  atomic.Int64
}

func newReadinessMesh(t *testing.T) *readinessMesh {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &readinessMesh{t: t, ctx: ctx, nodes: map[string]*readinessReplica{},
		conns: map[[2]string]*peerConnection{}, absentTo: map[[2]string]bool{}}
	for _, spec := range []struct {
		id string
		nt NodeType
	}{
		{"agent-a", NodeTypeAgent}, {"agent-b", NodeTypeAgent},
		{"planner-a", NodeTypePlanner}, {"workbench-a", NodeTypeWorkbench},
		{"bff-a", NodeTypeBFF}, {"bff-b", NodeTypeBFF}, {"bff-product-a", NodeTypeBFF},
		{"edge-a", NodeTypeEdge}, {"identity-a", NodeTypeIdentity},
	} {
		bus := events.NewBus(events.WithLogger(testLogger()))
		t.Cleanup(bus.Close)
		ident := &Identity{ID: spec.id, Type: spec.nt, Address: spec.id + ":50052"}
		pm := NewPeerManager(ident, testLogger())
		r := &readinessReplica{id: spec.id, bus: bus, pm: pm, bridge: NewEventBridge(ident, bus, pm, testLogger())}
		m.nodes[spec.id] = r
		m.order = append(m.order, spec.id)
	}
	return m
}

func (m *readinessMesh) nodeType(id string) NodeType {
	return m.nodes[id].bridge.identity.Type
}

// dial gives `from` an outbound connection to each of `to`, pumped into the
// target's inbound handler exactly as NodeServer.Stream drives it
// (stream_handler.go: HandleInbound, then ForwardInboundToPeers excluding the
// sender).
func (m *readinessMesh) dial(from string, to ...string) {
	src := m.nodes[from]
	for _, target := range to {
		dst := m.nodes[target]
		src.pm.Register(&nodev1.PeerInfo{
			NodeId:   target,
			NodeType: string(m.nodeType(target)),
			Address:  target + ":50052",
			Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		})
		conn := newPeerConnection(src.bridge.identity, target, target+":50052", testLogger())
		src.pm.AttachConnection(target, conn)
		m.conns[[2]string{from, target}] = conn
		go func(conn *peerConnection, dst *readinessReplica, sender string) {
			for {
				select {
				case <-m.ctx.Done():
					return
				case msg := <-conn.sendCh:
					if fwd := msg.GetEventForward(); fwd != nil {
						dst.bridge.HandleInbound(fwd)
						dst.bridge.ForwardInboundToPeers(fwd, sender)
					}
				}
			}
		}(conn, dst, from)
	}
}

// absent leaves the peer registered with no transport at all.
func (m *readinessMesh) absent(from, to string) {
	m.nodes[from].pm.DetachConnection(to)
	m.absentTo[[2]string{from, to}] = true
}

func (m *readinessMesh) detach(from, to string) { m.nodes[from].pm.DetachConnection(to) }

func (m *readinessMesh) attach(from, to string) {
	m.nodes[from].pm.AttachConnection(to, m.conns[[2]string{from, to}])
}

func (m *readinessMesh) failNextWrite(id string) {
	r := m.nodes[id]
	r.mu.Lock()
	r.failNext = true
	r.mu.Unlock()
}

// start puts the production recompute loop on every replica's bus, around a
// write that records which version of the cluster the pass read. A second,
// independent subscriber counts the registration events each bus delivers --
// the delivery evidence, kept apart from the loop so it cannot be confused
// with a safety-net pass.
func (m *readinessMesh) start() {
	for _, id := range m.order {
		r := m.nodes[id]
		for _, pattern := range events.GraphSubscriptionPatterns(memqlengine.WorkerRegistrationConcept, nil) {
			unsubscribe := r.bus.Subscribe(pattern, func(events.Event) { r.heard.Add(1) },
				events.WithSubscriberName("test:delivery"))
			if unsubscribe != nil {
				m.t.Cleanup(unsubscribe)
			}
		}
		memqlengine.StartReadinessRecomputeProbe(m.ctx, r.bus, func(context.Context) (int, error) {
			v := m.version.Load()
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.failNext && v > 0 {
				r.failNext = false
				r.failedTimes++
				return 0, errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")
			}
			r.seen = v
			return 1, nil
		}, memqlengine.ReadinessProbeTimings{
			Debounce:  20 * time.Millisecond,
			RetryBase: 60 * time.Millisecond,
			RetryMax:  120 * time.Millisecond,
			SafetyNet: 600 * time.Millisecond,
		})
	}
}

// change moves the cluster to version v and publishes the registration event
// on the writer's bus, then forwards it through the writer's bridge -- the two
// things a registration write does on the node that makes it.
func (m *readinessMesh) change(v int64, writer string) {
	m.version.Store(v)
	r := m.nodes[writer]
	evt := events.NewEvent(
		events.TopicNodeUpdated(memqlengine.WorkerRegistrationConcept),
		events.KindNodeUpdated,
		map[string]any{"id": memqlengine.WorkerRegistrationConcept + ":machine-1"},
	)
	r.bus.PublishSync(evt)
	r.bridge.onLocalEvent(evt)
}

func (m *readinessMesh) awaitConvergence(t *testing.T, v int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		behind := m.behind(v)
		if len(behind) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("readiness rows still behind version %d: %v (delivery: %v)", v, m.behind(v), m.deliveries())
}

func (m *readinessMesh) behind(v int64) []string {
	var out []string
	for _, id := range m.order {
		r := m.nodes[id]
		r.mu.Lock()
		seen := r.seen
		r.mu.Unlock()
		if seen < v {
			out = append(out, id)
		}
	}
	return out
}

func (m *readinessMesh) heard(id string) int32 { return m.nodes[id].heard.Load() }

func (m *readinessMesh) failures(id string) int {
	r := m.nodes[id]
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failedTimes
}

func (m *readinessMesh) deliveries() map[string]int32 {
	out := map[string]int32{}
	for _, id := range m.order {
		out[id] = m.heard(id)
	}
	return out
}
