package node

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// DEDUP AND LOOP PREVENTION ACROSS ARBITRARY TOPOLOGIES (memql#5341).
//
// Once every stream carries events both ways (memql#5340) every link is a
// two-way edge, so the mesh is full of cycles: a 2-cycle per dial, a triangle
// wherever a planner dials an agent that shares its parent. The rule that
// keeps that finite is D3 of the design record: publish a first sighting once,
// relay it once, relay a repeat only when it travelled a strictly shorter
// route, never back to the sender or the origin. These tests hold that rule
// on the shapes that break naive flooding, and then on random graphs.

// attachDialedPeer registers a peer and binds a dialed connection with no live
// stream behind it: its sends land on sendCh, which the test reads.
func attachDialedPeer(t *testing.T, pm *PeerManager, peerId string, nt NodeType) *peerConnection {
	t.Helper()
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: peerId, NodeType: string(nt), Address: peerId + ":50052"})
	pc := newPeerConnection(pm.identity, peerId, peerId+":50052", testLogger())
	pm.AttachConnection(peerId, pc)
	return pc
}

// attachAcceptedPeer registers a peer that opened a stream to this node and
// returns the push half, whose outbox the test reads instead of a drain.
func attachAcceptedPeer(t *testing.T, pm *PeerManager, peerId string, nt NodeType) *inboundStream {
	t.Helper()
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: peerId, NodeType: string(nt), Address: peerId + ":50052"})
	s := newInboundStream(peerId, func(*nodev1.NodeServerMessage) error { return nil }, testLogger())
	pm.attachInbound(peerId, s)
	t.Cleanup(s.close)
	return s
}

func drainOutbox(s *inboundStream) []*nodev1.EventForward {
	var out []*nodev1.EventForward
	for {
		select {
		case m := <-s.outbox:
			out = append(out, m.GetEventForward())
		default:
			return out
		}
	}
}

func newBridgeFor(t *testing.T, id string, nt NodeType) *EventBridge {
	t.Helper()
	bus := events.NewBus(events.WithLogger(testLogger()))
	t.Cleanup(bus.Close)
	ident := &Identity{ID: id, Type: nt, Address: id + ":50052"}
	return NewEventBridge(ident, bus, NewPeerManager(ident, testLogger()), testLogger())
}

const relayTopic = "graph.node.updated.v1:worker:registration"

// The same event down two streams -- the 2-cycle every dial now is -- is
// published once and relayed once, and the relay goes to neither the peer it
// came from nor the node it originated on.
func TestAnEventIsPublishedOnceAndRelayedOnce(t *testing.T) {
	eb := newBridgeFor(t, "bff-a", NodeTypeBFF)
	pm := eb.peerManager
	fromDialed := attachDialedPeer(t, pm, "agent-a", NodeTypeAgent)
	fromAccepted := attachAcceptedPeer(t, pm, "planner-a", NodeTypePlanner)
	origin := attachAcceptedPeer(t, pm, "workbench-a", NodeTypeWorkbench)
	onward := attachAcceptedPeer(t, pm, "edge-a", NodeTypeEdge)

	evt := &nodev1.EventForward{EventId: "evt-1", Topic: relayTopic, OriginNodeId: "workbench-a", Hops: 2}
	eb.ReceiveForward(evt, "agent-a")
	eb.ReceiveForward(evt, "planner-a")
	eb.ReceiveForward(evt, "agent-a")

	r := eb.MeshReport()
	if r.Heard != 1 || r.Duplicates != 2 || r.Relayed != 1 {
		t.Fatalf("heard=%d duplicates=%d relayed=%d, want 1, 2, 1", r.Heard, r.Duplicates, r.Relayed)
	}
	if got := drainSendCh(fromDialed); len(got) != 0 {
		t.Fatalf("relayed back to the peer it came from: %d copies", len(got))
	}
	if got := drainOutbox(origin); len(got) != 0 {
		t.Fatalf("relayed back to the node it originated on: %d copies", len(got))
	}
	// planner-a delivered a copy too, but only AFTER the first sighting was
	// relayed from agent-a -- so it got the relay, and that is the one copy it
	// may get.
	if got := drainOutbox(fromAccepted); len(got) != 1 {
		t.Fatalf("planner-a got %d copies, want exactly the one relay", len(got))
	}
	got := drainOutbox(onward)
	if len(got) != 1 {
		t.Fatalf("edge-a got %d copies, want 1", len(got))
	}
	if got[0].Hops != 3 || got[0].EventId != "evt-1" || got[0].OriginNodeId != "workbench-a" {
		t.Fatalf("relay = %+v, want hops 3 with the event's id and origin", got[0])
	}
}

// A copy that won the race the long way round is followed by one that came
// the short way. The short one is relayed again -- so the nodes beyond this
// one get the budget the short route left them -- and is not published again.
func TestAShorterRouteIsRelayedAgainButNotRepublished(t *testing.T) {
	eb := newBridgeFor(t, "bff-a", NodeTypeBFF)
	onward := attachDialedPeer(t, eb.peerManager, "edge-a", NodeTypeEdge)

	long := &nodev1.EventForward{EventId: "evt-race", Topic: relayTopic, OriginNodeId: "agent-z", Hops: 9}
	short := &nodev1.EventForward{EventId: "evt-race", Topic: relayTopic, OriginNodeId: "agent-z", Hops: 2}
	eb.ReceiveForward(long, "planner-a")
	eb.ReceiveForward(short, "agent-b")
	eb.ReceiveForward(short, "agent-c") // as short, not shorter: a plain duplicate

	got := drainSendCh(onward)
	if len(got) != 2 {
		t.Fatalf("edge-a got %d copies, want 2 (the long route, then the shorter one)", len(got))
	}
	if h := got[1].GetEventForward().Hops; h != 3 {
		t.Fatalf("the shorter route relayed at hops %d, want 3", h)
	}
	r := eb.MeshReport()
	if r.Heard != 1 || r.Relayed != 2 || r.Duplicates != 2 {
		t.Fatalf("heard=%d relayed=%d duplicates=%d, want 1, 2, 2", r.Heard, r.Relayed, r.Duplicates)
	}
}

// A node that originated an event drops every copy that comes back round, and
// none of them reads as a shorter route: the origin is distance 0.
func TestTheOriginNeverTakesItsOwnEventBack(t *testing.T) {
	eb := newBridgeFor(t, "agent-a", NodeTypeAgent)
	peer := attachDialedPeer(t, eb.peerManager, "bff-a", NodeTypeBFF)

	eb.onLocalEvent(events.NewEvent(relayTopic, events.KindNodeUpdated, map[string]any{"id": "x"}))
	sent := drainSendCh(peer)
	if len(sent) != 1 {
		t.Fatalf("origin sent %d copies to its one peer, want 1", len(sent))
	}
	back := sent[0].GetEventForward()
	if back.Hops != 1 {
		t.Fatalf("an origin sends hops 1 (the link it is about to travel), got %d", back.Hops)
	}
	back.Hops = 2
	eb.ReceiveForward(back, "bff-a")
	if r := eb.MeshReport(); r.Heard != 0 || r.Relayed != 0 || r.Originated != 1 {
		t.Fatalf("heard=%d relayed=%d originated=%d, want 0, 0, 1", r.Heard, r.Relayed, r.Originated)
	}
}

// ONE TRANSPORT PER PEER (D2): a live dialed stream first, then the newest open
// accepted stream, then a dialed connection that is reconnecting.
func TestOneTransportPerPeer(t *testing.T) {
	eb := newBridgeFor(t, "bff-a", NodeTypeBFF)
	pm := eb.peerManager
	dialed := attachDialedPeer(t, pm, "agent-a", NodeTypeAgent)
	older := attachAcceptedPeer(t, pm, "agent-a", NodeTypeAgent)
	newer := attachAcceptedPeer(t, pm, "agent-a", NodeTypeAgent)
	entry := pm.Get("agent-a")

	send := func() string {
		t.Helper()
		sendFn, transport, ok := pm.eventTarget(entry)
		if !ok {
			t.Fatal("no transport to a peer with three streams")
		}
		sendFn(&nodev1.EventForward{EventId: "e", Topic: relayTopic})
		switch {
		case len(drainSendCh(dialed)) == 1:
			return "dialed:" + transport
		case len(drainOutbox(newer)) == 1:
			return "newer:" + transport
		case len(drainOutbox(older)) == 1:
			return "older:" + transport
		}
		return "nowhere"
	}

	// The dialed connection has no live stream (it is backing off), so the
	// newest accepted stream wins.
	if got := send(); got != "newer:accepted" {
		t.Fatalf("with the dial reconnecting, sent on %s; want the newest accepted stream", got)
	}
	// A dialed stream that is up wins over everything.
	dialed.mu.Lock()
	dialed.current = &peerStream{done: make(chan struct{}), sendCh: make(chan *nodev1.NodeClientMessage, 1)}
	dialed.mu.Unlock()
	if got := send(); got != "dialed:dialed" {
		t.Fatalf("with the dial up, sent on %s; want the dialed stream", got)
	}
	dialed.mu.Lock()
	dialed.current = nil
	dialed.mu.Unlock()
	// The newest accepted stream ends: the older one is still open and serves.
	newer.close()
	if got := send(); got != "older:accepted" {
		t.Fatalf("with the newest accepted stream closed, sent on %s; want the older one", got)
	}
	// Every accepted stream gone: the reconnecting dial queues it, which is
	// what a dialed connection always did.
	older.close()
	if got := send(); got != "dialed:dialed" {
		t.Fatalf("with only a reconnecting dial left, sent on %s; want it", got)
	}
}

// A stream that ends late must release only itself: the replacement that
// reconnected under the same peer id keeps pushing.
func TestALateStreamEndReleasesOnlyItself(t *testing.T) {
	eb := newBridgeFor(t, "bff-a", NodeTypeBFF)
	pm := eb.peerManager
	old := attachAcceptedPeer(t, pm, "edge-a", NodeTypeEdge)
	replacement := attachAcceptedPeer(t, pm, "edge-a", NodeTypeEdge)

	pm.detachInbound("edge-a", old)
	old.close()

	sendFn, transport, ok := pm.eventTarget(pm.Get("edge-a"))
	if !ok || transport != "accepted" {
		t.Fatalf("after the old stream ended: ok=%v transport=%q, want the replacement", ok, transport)
	}
	sendFn(&nodev1.EventForward{EventId: "e", Topic: relayTopic})
	if got := drainOutbox(replacement); len(got) != 1 {
		t.Fatalf("the replacement stream got %d copies, want 1", len(got))
	}
}

// A full outbox drops the copy, counts it, and never blocks the sender: the
// bridge forwards from inside the local bus's dispatch.
func TestAFullOutboxDropsAndCounts(t *testing.T) {
	eb := newBridgeFor(t, "bff-a", NodeTypeBFF)
	stuck := attachAcceptedPeer(t, eb.peerManager, "edge-a", NodeTypeEdge)
	for i := 0; i < cap(stuck.outbox); i++ {
		stuck.outbox <- &nodev1.NodeServerMessage{}
	}
	eb.onLocalEvent(events.NewEvent(relayTopic, events.KindNodeUpdated, map[string]any{"id": "x"}))
	if r := eb.MeshReport(); r.Dropped != 1 || r.Originated != 1 {
		t.Fatalf("dropped=%d originated=%d, want 1 and 1", r.Dropped, r.Originated)
	}
}

// ---------------------------------------------------------------------------
// Arbitrary topologies
// ---------------------------------------------------------------------------

// TestFloodingIsExactlyOnceOnArbitraryTopologies builds random connected
// meshes -- random node types, identity included, duplicate links in both
// directions, cycles everywhere -- over a synchronous in-memory transport, has
// every node originate one event, and holds four properties:
//
//  1. every participant hears every other node's event EXACTLY ONCE;
//  2. identity hears none;
//  3. delivery TERMINATES -- the wire drains with nothing left to send;
//  4. with copies delivered in arrival order (FIFO, which is what the real
//     transport approximates), every node relays each event at most once, so
//     the copies per event never exceed one per peer per node.
//
// Then it replays each mesh with copies delivered in RANDOM order -- the
// adversarial race -- where 1, 2 and 3 must still hold and the shorter-route
// relay is what keeps them holding.
//
// Meshes stay at twelve nodes or fewer, so no simple path is longer than the
// hop budget and "every participant hears" is a promise the budget cannot
// break; TestTheHopBudgetIsSixteenLinks pins the budget on its own.
func TestFloodingIsExactlyOnceOnArbitraryTopologies(t *testing.T) {
	rng := rand.New(rand.NewSource(5341))
	for trial := 0; trial < 120; trial++ {
		spec := randomMesh(rng, 2+rng.Intn(11))
		for _, order := range []string{"fifo", "random"} {
			name := fmt.Sprintf("trial-%d-%s-%d-nodes", trial, order, len(spec.types))
			sim := buildSimMesh(t, spec)
			sim.originateFromEach()
			sim.run(order, rng)
			sim.assertExactlyOnce(t, name)
			if order == "fifo" {
				sim.assertCopiesBounded(t, name)
			}
		}
	}
}

type meshSpec struct {
	types []NodeType
	dials [][2]int // from -> to
}

// randomMesh returns a connected mesh of n nodes: a random spanning tree (so
// every node has at least one stream) plus random extra dials, including the
// reverse of an existing one. A node's type is random, identity included --
// but a mesh never gives an identity node's only neighbours no other route,
// since identity relays nothing and the property is about participants.
func randomMesh(rng *rand.Rand, n int) meshSpec {
	kinds := []NodeType{NodeTypeBFF, NodeTypeAgent, NodeTypePlanner, NodeTypeWorkbench, NodeTypeEdge, NodeTypeMCP}
	spec := meshSpec{types: make([]NodeType, n)}
	for i := range spec.types {
		spec.types[i] = kinds[rng.Intn(len(kinds))]
	}
	// At most one identity node, never the first (which roots the tree), and
	// only as a leaf of the spanning tree: that keeps the participants
	// connected among themselves, which is the only topology in which "every
	// participant hears" is a promise the transport can keep.
	identityAt := -1
	if n > 2 && rng.Intn(3) == 0 {
		identityAt = 1 + rng.Intn(n-1)
		spec.types[identityAt] = NodeTypeIdentity
	}
	order := rng.Perm(n)
	// Put the identity node last in the attach order so nothing hangs off it.
	for i, v := range order {
		if v == identityAt {
			order = append(append(order[:i:i], order[i+1:]...), v)
			break
		}
	}
	for i := 1; i < n; i++ {
		child := order[i]
		var parent int
		for {
			parent = order[rng.Intn(i)]
			if parent != identityAt {
				break
			}
		}
		if rng.Intn(2) == 0 {
			spec.dials = append(spec.dials, [2]int{child, parent})
		} else {
			spec.dials = append(spec.dials, [2]int{parent, child})
		}
	}
	for extra := rng.Intn(n + 1); extra > 0; extra-- {
		a, b := rng.Intn(n), rng.Intn(n)
		if a == b || a == identityAt || b == identityAt {
			continue
		}
		spec.dials = append(spec.dials, [2]int{a, b})
	}
	return spec
}

// simMesh is the synchronous transport: every dial is a dialed connection on
// one side and an accepted stream on the other, and run() moves copies between
// them by hand, so the order of delivery is the test's to choose.
type simMesh struct {
	ids     []string
	types   []NodeType
	bridges []*EventBridge
	links   []simLink
	copies  int
}

type simLink struct {
	from, to int
	dialed   *peerConnection // on `from`, toward `to`
	accepted *inboundStream  // on `to`, toward `from`
}

func buildSimMesh(t *testing.T, spec meshSpec) *simMesh {
	t.Helper()
	sim := &simMesh{types: spec.types}
	for i, nt := range spec.types {
		id := fmt.Sprintf("n%02d-%s", i, nt)
		sim.ids = append(sim.ids, id)
		sim.bridges = append(sim.bridges, newBridgeFor(t, id, nt))
	}
	for _, d := range spec.dials {
		from, to := sim.bridges[d[0]], sim.bridges[d[1]]
		dialed := attachDialedPeer(t, from.peerManager, sim.ids[d[1]], sim.types[d[1]])
		accepted := attachAcceptedPeer(t, to.peerManager, sim.ids[d[0]], sim.types[d[0]])
		sim.links = append(sim.links, simLink{from: d[0], to: d[1], dialed: dialed, accepted: accepted})
	}
	return sim
}

// originateFromEach has every node put one event on the mesh. Heard counts
// come from each bridge's own report, which is synchronous with delivery; a
// first sighting is counted once by construction, so "heard every foreign
// event" is exactly "heard each of them once".
func (s *simMesh) originateFromEach() {
	for _, eb := range s.bridges {
		eb.onLocalEvent(events.NewEvent(relayTopic, events.KindNodeUpdated,
			map[string]any{"id": "v1:worker:registration:" + eb.identity.ID}))
	}
}

type simCopy struct {
	to   int
	from int
	evt  *nodev1.EventForward
}

// pending collects every copy queued on the wire, in link order.
func (s *simMesh) pending() []simCopy {
	var out []simCopy
	for _, l := range s.links {
		for _, m := range drainSendCh(l.dialed) {
			out = append(out, simCopy{to: l.to, from: l.from, evt: m.GetEventForward()})
		}
		for _, e := range drainOutbox(l.accepted) {
			out = append(out, simCopy{to: l.from, from: l.to, evt: e})
		}
	}
	return out
}

func (s *simMesh) run(order string, rng *rand.Rand) {
	queue := s.pending()
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1_000_000 {
			panic("flooding did not terminate")
		}
		var next simCopy
		if order == "random" {
			i := rng.Intn(len(queue))
			next = queue[i]
			queue = append(queue[:i], queue[i+1:]...)
		} else {
			next, queue = queue[0], queue[1:]
		}
		s.copies++
		s.bridges[next.to].ReceiveForward(next.evt, s.ids[next.from])
		queue = append(queue, s.pending()...)
	}
}

func (s *simMesh) assertExactlyOnce(t *testing.T, name string) {
	t.Helper()
	for i, eb := range s.bridges {
		r := eb.MeshReport()
		want := int64(0)
		if takesMeshEvents(s.types[i]) {
			// Every other node's event: participants' and identity's alike.
			want = int64(len(s.bridges) - 1)
		}
		if r.Heard != want {
			t.Fatalf("%s: %s heard %d events, want %d\n%s", name, s.ids[i], r.Heard, want, s.describe())
		}
		if r.HopLimited != 0 {
			t.Fatalf("%s: %s hop-limited %d copies in a mesh of %d nodes", name, s.ids[i], r.HopLimited, len(s.ids))
		}
	}
}

// assertCopiesBounded holds the FIFO property: each node relays each event at
// most once, so every node sends at most one copy per peer per event.
func (s *simMesh) assertCopiesBounded(t *testing.T, name string) {
	t.Helper()
	peers := make([]map[int]bool, len(s.bridges))
	for i := range peers {
		peers[i] = map[int]bool{}
	}
	for _, l := range s.links {
		peers[l.from][l.to] = true
		peers[l.to][l.from] = true
	}
	bound := 0
	for i := range s.bridges {
		for p := range peers[i] {
			if takesMeshEvents(s.types[p]) {
				bound++
			}
		}
	}
	bound *= len(s.bridges) // per event, one event per origin
	if s.copies > bound {
		t.Fatalf("%s: %d copies on the wire, bound %d (one per peer per node per event)", name, s.copies, bound)
	}
	for i, eb := range s.bridges {
		if r := eb.MeshReport(); r.Relayed > int64(len(s.bridges)-1) {
			t.Fatalf("%s: %s relayed %d times for %d foreign events", name, s.ids[i], r.Relayed, len(s.bridges)-1)
		}
	}
}

func (s *simMesh) describe() string {
	var lines []string
	for _, l := range s.links {
		lines = append(lines, fmt.Sprintf("  %s -> %s", s.ids[l.from], s.ids[l.to]))
	}
	sort.Strings(lines)
	return "links:\n" + strings.Join(lines, "\n")
}

// THE HOP BUDGET IS SIXTEEN LINKS (D4). On a chain of twenty nodes an event
// from one end is heard by exactly the sixteen nodes within sixteen links, the
// sixteenth counts one copy it would not relay, and nothing beyond hears it.
// A chain is the one shape that can exhaust the budget; it is pinned here so a
// change to the budget is a decision a test names rather than a drift.
func TestTheHopBudgetIsSixteenLinks(t *testing.T) {
	const n = 20
	spec := meshSpec{types: make([]NodeType, n)}
	for i := range spec.types {
		spec.types[i] = NodeTypeAgent
		if i > 0 {
			spec.dials = append(spec.dials, [2]int{i, i - 1})
		}
	}
	sim := buildSimMesh(t, spec)
	sim.bridges[0].onLocalEvent(events.NewEvent(relayTopic, events.KindNodeUpdated, map[string]any{"id": "far"}))
	sim.run("fifo", nil)

	for i := 1; i < n; i++ {
		r := sim.bridges[i].MeshReport()
		want := int64(0)
		if i <= meshMaxHops {
			want = 1
		}
		if r.Heard != want {
			t.Fatalf("node %d links out heard %d, want %d", i, r.Heard, want)
		}
		wantLimited := int64(0)
		if i == meshMaxHops {
			wantLimited = 1
		}
		if r.HopLimited != wantLimited {
			t.Fatalf("node %d links out hop-limited %d, want %d", i, r.HopLimited, wantLimited)
		}
	}
}
