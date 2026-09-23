package node

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/grpc"
)

// A BROADCAST REACHES EVERY MESH PARTICIPANT, WHOEVER DIALED WHOM
// (memql#5338, the gate memql#5339 asked for).
//
// The production evidence: the product bff and the edge pods had logged 6
// `event trigger fired` lines since boot, against 430-1084 on the engine bff,
// agent and planner. They were not quiet; they were deaf. A forward went only
// along a node's OUTBOUND dials, nothing ever pushed an event down a stream a
// node had ACCEPTED, so a node nobody dials heard nothing -- and "nobody dials
// it" described the product bff, the edge and mcp in the cloud's own topology.
//
// This drives the REAL transport over loopback -- NodeServer streams,
// ParentConnector and WorkerDialer dials, one PeerManager and EventBridge per
// replica -- in that topology, plus the two shapes a TTL of three could not
// reach: an edge on each engine bff (four hops apart) and an mcp whose
// discovered parent is an edge (five from the far edge). Every replica
// originates one broadcast, and every participant must hear every one of them
// EXACTLY ONCE, while identity hears none.
//
// Against the pre-epic transport it fails naming the island. That is the
// point of writing it first: a gate that has never been red has never been
// shown to measure anything.
func TestABroadcastReachesEveryMeshParticipant(t *testing.T) {
	m := newWireMesh(t)
	m.cloudTopology()
	m.start()
	m.awaitLinks()

	probes := m.broadcastFromEach(registrationTopic)
	m.awaitDelivery(probes, 10*time.Second)
	// Let any late duplicate land before counting: a copy that arrives after
	// the last first-sighting is exactly what "exactly once" must survive.
	time.Sleep(300 * time.Millisecond)
	m.assertExactlyOnce(probes)
}

// registrationTopic is a broadcast-routed topic with a real consumer on every
// node: the readiness recompute loop subscribes to it (memql#5259), which is
// why an island's readiness row never moved.
var registrationTopic = events.TopicNodeUpdated("v1:worker:registration")

// ---------------------------------------------------------------------------
// The loopback mesh
// ---------------------------------------------------------------------------

// wireNode is one replica: its own bus, peer table, bridge and NodeService
// server, and -- once started -- its own dials.
type wireNode struct {
	id       string
	identity *Identity
	bus      *events.Bus
	pm       *PeerManager
	bridge   *EventBridge
	lis      net.Listener
	server   *grpc.Server

	seeds  []WorkerTarget // what this node's WorkerDialer dials
	parent string         // the node its ParentConnector dials

	mu    sync.Mutex
	heard map[string]int // probe id -> times it arrived on this node's bus
}

type wireMesh struct {
	t     *testing.T
	ctx   context.Context
	nodes map[string]*wireNode
	order []string
	links [][2]string // every declared dial, from -> to
}

func newWireMesh(t *testing.T) *wireMesh {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &wireMesh{t: t, ctx: ctx, nodes: map[string]*wireNode{}}
}

// node adds a replica listening on a loopback port it advertises as its own
// address, the way a pod advertises $(POD_IP):port.
func (m *wireMesh) node(id string, nt NodeType) *wireNode {
	m.t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		m.t.Fatalf("listen for %s: %v", id, err)
	}
	bus := events.NewBus(events.WithLogger(testLogger()))
	m.t.Cleanup(bus.Close)
	ident := &Identity{ID: id, Type: nt, Address: lis.Addr().String()}
	pm := NewPeerManager(ident, testLogger())
	n := &wireNode{
		id: id, identity: ident, bus: bus, pm: pm,
		bridge: NewEventBridge(ident, bus, pm, testLogger()),
		lis:    lis, heard: map[string]int{},
	}
	m.nodes[id] = n
	m.order = append(m.order, id)
	return n
}

// dials gives `from` a WorkerDialer seeded with each target -- the bff's
// dials to identity, workbench, agents and planners; a planner's to agents; an
// agent's to workbench.
func (m *wireMesh) dials(from string, to ...string) {
	src := m.nodes[from]
	for _, target := range to {
		dst := m.nodes[target]
		src.seeds = append(src.seeds, WorkerTarget{NodeType: dst.identity.Type, Address: dst.identity.Address})
		m.links = append(m.links, [2]string{from, target})
	}
}

// parent gives `child` a ParentConnector dialing `parent` -- MEMQL_PARENT_ADDRESS.
func (m *wireMesh) parent(child, parent string) {
	c := m.nodes[child]
	c.parent = parent
	c.identity.ParentAddress = m.nodes[parent].identity.Address
	m.links = append(m.links, [2]string{child, parent})
}

// cloudTopology is the dial graph memql#5316's analysis measured in the cloud
// (section 1.3), every parent dial landing on bff-a, plus the edge-on-bff-b
// and mcp-on-an-edge shapes that put participants four and five hops apart.
func (m *wireMesh) cloudTopology() {
	m.node("bff-a", NodeTypeBFF)
	m.node("bff-b", NodeTypeBFF)
	m.node("bff-product-a", NodeTypeBFF)
	m.node("agent-a", NodeTypeAgent)
	m.node("agent-b", NodeTypeAgent)
	m.node("planner-a", NodeTypePlanner)
	m.node("workbench-a", NodeTypeWorkbench)
	m.node("edge-a", NodeTypeEdge)
	m.node("edge-b", NodeTypeEdge)
	m.node("mcp-a", NodeTypeMCP)
	m.node("identity-a", NodeTypeIdentity)

	for _, bff := range []string{"bff-a", "bff-b", "bff-product-a"} {
		m.dials(bff, "identity-a", "workbench-a", "agent-a", "agent-b", "planner-a")
	}
	m.parent("agent-a", "bff-a")
	m.dials("agent-a", "workbench-a")
	m.parent("agent-b", "bff-a")
	m.dials("agent-b", "workbench-a")
	m.parent("planner-a", "bff-a")
	m.dials("planner-a", "agent-a", "agent-b")
	m.parent("workbench-a", "bff-a")
	m.parent("edge-a", "bff-a")
	m.parent("edge-b", "bff-b")
	m.parent("mcp-a", "edge-b")
}

// start serves every node, then opens every dial. Servers first, so a dial
// never spends its first backoff on a port nobody is listening on yet.
func (m *wireMesh) start() {
	m.t.Helper()
	for _, id := range m.order {
		n := m.nodes[id]
		n.pm.Start(m.ctx)
		n.bridge.Start(m.ctx)
		n.server = grpc.NewServer()
		nodev1.RegisterNodeServiceServer(n.server, n.service())
		go func(n *wireNode) { _ = n.server.Serve(n.lis) }(n)
		m.t.Cleanup(n.server.Stop)
		m.t.Cleanup(func() { n.bridge.Stop(context.Background()); n.pm.Stop(context.Background()) })

		for _, pattern := range []string{registrationTopic} {
			unsubscribe := n.bus.Subscribe(pattern, func(e events.Event) {
				probe, _ := e.Payload["probe"].(string)
				if probe == "" {
					return
				}
				n.mu.Lock()
				n.heard[probe]++
				n.mu.Unlock()
			}, events.WithSubscriberName("test:mesh-delivery"))
			if unsubscribe != nil {
				m.t.Cleanup(unsubscribe)
			}
		}
	}
	for _, id := range m.order {
		n := m.nodes[id]
		if n.parent != "" {
			pc := NewParentConnector(n.identity, n.pm, testLogger())
			n.wireParent(pc)
			pc.Start(m.ctx)
			m.t.Cleanup(func() { pc.Stop(context.Background()) })
		}
		if len(n.seeds) > 0 {
			wd := NewWorkerDialer(n.identity, n.pm, nil, nil, n.seeds, testLogger())
			wd.Start(m.ctx)
			m.t.Cleanup(func() { wd.Stop(context.Background()) })
		}
	}
}

// awaitLinks waits until every declared dial has completed its handshake: the
// dialer holds a live connection bound to the peer it reached.
func (m *wireMesh) awaitLinks() {
	m.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var missing []string
		for _, l := range m.links {
			from := m.nodes[l[0]]
			if entry := from.pm.Get(l[1]); entry == nil {
				missing = append(missing, l[0]+"->"+l[1])
			} else if _, ok := from.pm.sendTarget(entry); !ok {
				missing = append(missing, l[0]+"->"+l[1])
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("mesh never came up; dials without a live connection: %v", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// broadcastFromEach publishes one broadcast-routed event on every replica's
// local bus -- what a graph write does on the node that makes it -- and
// returns the probe ids, one per origin.
func (m *wireMesh) broadcastFromEach(topic string) map[string]string {
	probes := map[string]string{}
	for _, id := range m.order {
		probe := "probe-from-" + id
		probes[id] = probe
		m.nodes[id].bus.Publish(events.NewEvent(topic, events.KindNodeUpdated,
			map[string]any{"id": "v1:worker:registration:" + probe, "probe": probe}))
	}
	return probes
}

// participant reports whether a node takes mesh events. Identity is the one
// node type that does not (meshEventParticipants), and it must stay that way.
func participant(n *wireNode) bool { return n.identity.Type != NodeTypeIdentity }

// missingDeliveries lists, per receiver, the origins whose probe it has not
// heard. A node always hears its own probe on its own bus.
func (m *wireMesh) missingDeliveries(probes map[string]string) map[string][]string {
	missing := map[string][]string{}
	for _, rid := range m.order {
		r := m.nodes[rid]
		for _, oid := range m.order {
			if oid != rid && !participant(r) {
				continue
			}
			r.mu.Lock()
			got := r.heard[probes[oid]]
			r.mu.Unlock()
			if got == 0 {
				missing[rid] = append(missing[rid], oid)
			}
		}
	}
	return missing
}

func (m *wireMesh) awaitDelivery(probes map[string]string, within time.Duration) {
	m.t.Helper()
	deadline := time.Now().Add(within)
	for {
		missing := m.missingDeliveries(probes)
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("THE MESH ISLAND: these replicas did not hear a broadcast from these origins "+
				"within %s -- a node is reached only if the transport carries events down every "+
				"stream, in both directions, far enough:\n%s", within, describeMissing(m.order, missing))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertExactlyOnce holds the other half of the contract: every participant
// published every probe ONCE, and identity published none but its own.
func (m *wireMesh) assertExactlyOnce(probes map[string]string) {
	m.t.Helper()
	var faults []string
	for _, rid := range m.order {
		r := m.nodes[rid]
		for _, oid := range m.order {
			r.mu.Lock()
			got := r.heard[probes[oid]]
			r.mu.Unlock()
			want := 1
			if oid != rid && !participant(r) {
				want = 0
			}
			if got != want {
				faults = append(faults, fmt.Sprintf("%s heard %s's probe %d time(s), want %d", rid, oid, got, want))
			}
		}
	}
	if len(faults) > 0 {
		sort.Strings(faults)
		m.t.Fatalf("delivery was not exactly once:\n  %s", strings.Join(faults, "\n  "))
	}
}

func describeMissing(order []string, missing map[string][]string) string {
	var b strings.Builder
	for _, id := range order {
		if origins := missing[id]; len(origins) > 0 {
			fmt.Fprintf(&b, "  %-14s missed %d of %d: %s\n", id, len(origins), len(order), strings.Join(origins, ", "))
		}
	}
	return b.String()
}

// service is the NodeService a replica's server registers -- the same
// nodeService NodeServer builds, over this replica's peer table and bridge.
func (n *wireNode) service() nodev1.NodeServiceServer {
	return &nodeService{logger: testLogger(), identity: n.identity, peerManager: n.pm, eventInbound: n.bridge}
}

// wireParent connects a ParentConnector to this replica's bridge, as the
// bootstraps do.
func (n *wireNode) wireParent(pc *ParentConnector) { pc.SetEventInbound(n.bridge) }
