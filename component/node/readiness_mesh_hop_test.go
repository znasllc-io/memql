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
// WHO THE EVENT DOES NOT REACH (the delivery evidence)
// ===========================================================================
// When memql#5259 was filed a forward went only along OUTBOUND dials, a relay
// went at most three hops, and nothing pushed server->client -- so the edge,
// the product bff and (depending on round-robin) the other shared bff heard
// nothing. memql#5338 made every stream carry events both ways, far enough to
// cross the mesh (mesh_delivery_test.go is that gate). Two ways are left for a
// readiness participant to miss the broadcast, and this test keeps both:
//
//   - identity is excluded from every broadcast (meshEventParticipants), by
//     design, even though every bff dials it -- and nothing may widen that,
//     because it would fire identity's subscribers on traffic it has never
//     seen;
//   - a node whose every stream is DOWN at that moment -- here the edge, whose
//     one stream is cut across the change -- misses it, and nothing queues
//     the event for it.
//
// The recompute loop does not need either of them to hear: the node that
// WROTE the registration hears its own event on its local bus, the fold sets
// aside a row whose cluster-scoped lanes lag the freshest one, and every other
// node's safety-net pass converges its row within one period.
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

	// A STREAM THAT IS DOWN AT THE MOMENT OF THE CHANGE: the edge's only
	// stream -- its parent dial to bff-a -- is cut in both directions across
	// the change and restored after it.
	m.cut("edge-a", "bff-a")
	// A FAILED READINESS WRITE: the edge's first pass after the change is
	// refused, the way a saturated database refused it.
	m.failNextWrite("edge-a")

	m.start()

	// THE PAIRING. The agent holding the machine's stream writes the
	// registration: the event lands on ITS bus, and its bridge forwards it.
	m.change(1, "agent-a")
	time.Sleep(50 * time.Millisecond)
	m.restore("edge-a", "bff-a")

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
	// The cut stream lost the event: nothing buffers a mesh forward for a
	// peer with no live stream.
	if n := m.heard("edge-a"); n != 0 {
		t.Fatalf("edge-a heard %d event(s) with its only stream cut", n)
	}
	// The failed write was retried rather than abandoned.
	if m.failures("edge-a") == 0 {
		t.Fatal("negative control failed: the edge's write was never refused, so the retry proved nothing")
	}

	// WHO MISSED IT. When memql#5338 was filed this list was
	// {agent-b, bff-b, bff-product-a, edge-a, identity-a, planner-a}: the
	// island. Server-to-client push (memql#5340) turned it over, as this
	// comment said it would, and the convergence above passed without an
	// edit. What is left is the design (identity) and a stream that was down.
	var island []string
	for _, id := range m.order {
		if m.heard(id) == 0 {
			island = append(island, id)
		}
	}
	sort.Strings(island)
	want := []string{"edge-a", "identity-a"}
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
	t       *testing.T
	ctx     context.Context
	nodes   map[string]*readinessReplica
	order   []string
	conns   map[[2]string]*peerConnection // from -> to: the dialed half
	inbound map[[2]string]*inboundStream  // from -> to: the half `to` pushes down
	version atomic.Int64
}

func newReadinessMesh(t *testing.T) *readinessMesh {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &readinessMesh{t: t, ctx: ctx, nodes: map[string]*readinessReplica{},
		conns: map[[2]string]*peerConnection{}, inbound: map[[2]string]*inboundStream{}}
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

// dial gives `from` a stream to each of `to`, BOTH halves of it, exactly as
// the real transport now drives one:
//
//   - the dialed half: an outbound connection on `from`, pumped into the
//     target's one arrival path (ReceiveForward, as NodeServer.Stream calls
//     it);
//   - the pushed half (memql#5338): `to` holds `from` as a peer with an
//     accepted stream whose sends land on `from`'s arrival path, as the
//     ParentConnector and the WorkerDialer deliver them.
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
						dst.bridge.ReceiveForward(fwd, sender)
					}
				}
			}
		}(conn, dst, from)

		dst.pm.Register(&nodev1.PeerInfo{
			NodeId:   from,
			NodeType: string(m.nodeType(from)),
			Address:  from + ":50052",
			Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		})
		pushed := newInboundStream(from, func(sender string) func(*nodev1.NodeServerMessage) error {
			return func(msg *nodev1.NodeServerMessage) error {
				if fwd := msg.GetEventForward(); fwd != nil {
					src.bridge.ReceiveForward(fwd, sender)
				}
				return nil
			}
		}(target), testLogger())
		dst.pm.attachInbound(from, pushed)
		go pushed.run()
		m.t.Cleanup(pushed.close)
		m.inbound[[2]string{from, target}] = pushed
	}
}

// cut takes the whole from->to stream down, both halves: the state of a
// stream mid-reconnect, and since memql#5338 the one way left for a
// participant to miss a broadcast.
func (m *readinessMesh) cut(from, to string) {
	m.nodes[from].pm.DetachConnection(to)
	m.nodes[to].pm.detachInbound(from, m.inbound[[2]string{from, to}])
}

// restore brings a cut stream back, both halves.
func (m *readinessMesh) restore(from, to string) {
	m.nodes[from].pm.AttachConnection(to, m.conns[[2]string{from, to}])
	m.nodes[to].pm.attachInbound(from, m.inbound[[2]string{from, to}])
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
