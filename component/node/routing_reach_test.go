package node

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// TestSavedViewCrossesTwoBffReplicas is the marquee proof for memql#4542.
//
// # Why this test is in-process rather than in test/clustere2e
//
// The bug it pins needs two replicas to reproduce, and the obvious home for
// a two-replica proof is the cluster harness. memql#4352 is the reason it is
// not there: a live-cluster gate is SKIPPED on every CI lane and on every
// developer machine, and a gate skipped by default cannot be the thing
// standing between a feature and the bug it prevents. So this wires the real
// components to each other in one process and leaves out only the gRPC
// transport between them.
//
// # What is real here
//
// Everything that decides. Replica A's own events.Bus subscription, the real
// EventBridge.onLocalEvent, the real evaluateRouting against the real
// defaultRoutingRules, the real forwardToPeers against a real PeerManager
// with a registered peer -- and on the far side, the real HandleInbound with
// its dedup and TTL checks, publishing onto replica B's real bus. The only
// stand-in is the peerConnection's channel, which is what the gRPC stream
// would have drained.
//
// Deleting the v1:worker:registration rules makes this fail. That is the whole
// specification: a machine registered through one replica has to appear in a
// Fleet list served by another, and before memql#4542 it never did -- which
// looked like a bug in the list, because the list's subscription was working
// perfectly and receiving nothing.
//
// IT WAS v1:portalviews:view UNTIL EPIC memql#4984, and the swap is worth
// recording rather than doing quietly. Saved views were the marquee case for
// the memql#4542 sweep and the concept went with the portal, but the MECHANISM
// this exercises did not: registrations are written on the agent node and read
// on the page the bff serves, which is the same shape and a live one. A test
// whose subject is deleted should be re-pointed at something that still
// matters or removed outright -- never left asserting over a concept nothing
// writes, where it would pass forever while measuring nothing.
func TestABrowserFacingRowCrossesTwoReplicas(t *testing.T) {
	replicaA, replicaB, deliver := twoReplicaMesh(t)

	// The Fleet machines list on a browser attached to replica B.
	got := make(chan events.Event, 4)
	unsub := replicaB.bus.Subscribe("graph.node.created.v1:worker:registration", func(e events.Event) {
		got <- e
	}, events.WithSubscriberName("test:fleet"))
	defer unsub()

	// The registration, landing on replica A.
	replicaA.bus.Publish(events.NewEvent(
		"graph.node.created.v1:worker:registration",
		events.KindNodeCreated,
		map[string]any{"id": "reg-1", "displayName": "Ada's laptop", "ownerUserId": "u1"},
	))

	deliver(t)

	select {
	case e := <-got:
		if name, _ := e.Payload["displayName"].(string); name != "Ada's laptop" {
			t.Errorf("the event crossed but its payload did not: displayName = %v, want %q",
				e.Payload["displayName"], "Ada's laptop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a registration written on replica A never reached replica B.\n" +
			"That is the memql#4542 bug exactly: with two replicas (the default " +
			"topology) the list is correct on load and frozen afterwards.\n" +
			"Check the graph.node.*.v1:worker:registration forward rules in routing.go.")
	}
}

// TestBrowserFacingConceptsForwardWithTheVerbsTheirSurfacesSubscribeTo pins
// the memql#4542 table at the level a reader can check against a client:
// concept, verb, and the surface that needs it.
//
// IT IS NOW THE ONLY GATE ON THIS. A repo-root companion
// (portal_subscription_routing_test.go, memql#4543) used to derive the same
// claims from the PORTAL's source and notice a new subscription this list did
// not mention; it went with the portal in epic memql#4984, and its own doc
// said only this direction would survive the portal being rewritten. So this
// list is no longer cross-checked against any client, and a NEW OS
// subscription over an unforwarded concept will present the way they all do:
// correct on load, frozen after, and looking like a bug in the surface.
func TestBrowserFacingConceptsForwardWithTheVerbsTheirSurfacesSubscribeTo(t *testing.T) {
	cases := []struct {
		concept string
		verbs   []string
		surface string
	}{
		{"v1:agents:agent", []string{"created", "updated", "deleted"}, "the Agents view"},
		{"v1:library:artifact", []string{"created", "updated", "deleted"}, "the Artifacts page and Nexus's artifact slot"},
		{"v1:library:file", []string{"created", "updated"}, "the Artifacts page's backing file row"},
		{"v1:authoring:bundle", []string{"created", "updated", "deleted"}, "Nexus Constructs"},
		{"v1:authoring:construct", []string{"created", "updated", "deleted"}, "Nexus Constructs"},
		{"v1:authoring:dependencyEdge", []string{"created", "updated", "deleted"}, "Nexus's dependency edges"},
		{"v1:identity:invitation", []string{"created", "updated"}, "pending invitations on People"},
		{"v1:identity:account", []string{"created"}, "the Home accounts tile"},
		{"v1:identity:auditEvent", []string{"created"}, "the Home audit tile"},
		{"v1:library:artifact", []string{"created", "updated", "deleted"}, "a chat surface -- deletes were the hole"},
		{"v1:worker:registration", []string{"created", "updated", "deleted"}, "the Fleet machines page -- deletes were the hole"},
		{"v1:workbench:workspace", []string{"created", "updated", "deleted"}, "the Fleet workbenches page -- deletes were the hole"},
		{"v1:worker:routingPolicy", []string{"created", "updated", "deleted"}, "the Fleet routing policy editor"},
		// The campaigns operator rows (epic memql#4827). The OS Campaigns
		// app is the surface, and it is being built in the same epic these
		// rules landed in -- so unlike every row above, the claim here is
		// "this rule exists FOR that surface", not "that surface is
		// already subscribing today". Recorded anyway, and recorded as
		// what it is: the rules were added ahead of the app precisely so
		// the app is live on arrival rather than correct-on-load-and-frozen,
		// and without an entry here a cleanup could delete them between now
		// and then with nothing to notice.
		//
		// CREATED + UPDATED only. Nothing hard-deletes a campaigns row --
		// the lifecycle is archive-not-delete -- and nothing in the engine
		// publishes graph.node.deleted for any concept at all, so a deleted
		// verb here would be asserting about surface nothing sends.
		{"v1:campaigns:campaign", []string{"created", "updated"}, "the OS Campaigns app's campaign list"},
		{"v1:campaigns:audience", []string{"created", "updated"}, "the OS Campaigns app's Audiences section"},
		{"v1:campaigns:template", []string{"created", "updated"}, "the OS Campaigns app's Templates section"},
		{"v1:campaigns:senderIdentity", []string{"created", "updated"}, "the sender-identity picker and its verification state"},
		{"v1:campaigns:emailRule", []string{"created", "updated"}, "the event-email rules surface"},

		// THE WORK SPINE (design record 2026-09-05-work-spine-design.md,
		// section D "Live feeds"). Like the campaigns rows above, these
		// rules land ahead of their surface: sub-project B builds the OS
		// Nexus over them, and without an entry here a cleanup could
		// delete them in between with nothing to notice. modelCall and
		// observation are deliberately absent -- on-demand reads,
		// excluded on volume grounds exactly as v1:worker:invocation is.
		{"v1:work:goal", []string{"created", "updated", "deleted"}, "the OS Nexus goal list (sub-project B) is a live feed"},
		{"v1:work:run", []string{"created", "updated", "deleted"}, "a run's status flips on the node that runs it, and the person watching is on the bff"},
		{"v1:work:step", []string{"created", "updated", "deleted"}, "the run rail is one LiveList over step rows"},
		{"v1:work:approval", []string{"created", "updated", "deleted"}, "a person must see an approval arrive without polling"},
	}

	for _, c := range cases {
		for _, verb := range c.verbs {
			topic := GraphEventTopic(verb, c.concept)
			if !ForwardsGraphEvent(topic) {
				t.Errorf("%s is not forwarded.\n  Surface that goes dark: %s\n  Add a reasoned rule in component/node/routing.go, or a reasoned exclusion in RoutingExclusions().",
					topic, c.surface)
			}
		}
	}
}

// TestRecordedExclusionsStayExcluded is the other half of the table. An
// exclusion that quietly starts forwarding is not a bug the way a missing
// rule is -- nothing goes dark -- but it means the reason nobody wrote it is
// no longer true, and the two entries here are the ones with a COST attached:
// forwarding either would put a per-request or per-recipient row on the mesh.
func TestRecordedExclusionsStayExcluded(t *testing.T) {
	for _, ex := range RoutingExclusions() {
		if strings.TrimSpace(ex.Reason) == "" {
			t.Errorf("exclusion %q carries no reason. An exclusion without a why is a hole with paperwork -- the next reader cannot tell it from an oversight somebody silenced.", ex.Pattern)
		}
	}

	// Spelled-out topics rather than the patterns, so a pattern that stopped
	// matching what it names would show up here rather than pass vacuously.
	for _, topic := range []string{
		"graph.node.updated.v1:identity:user",
		"graph.node.created.v1:worker:invocation",
		"graph.node.created.v1:campaigns:delivery",
		"graph.node.created.v1:campaigns:engagementEvent",
		"graph.node.created.v1:campaigns:recipient",
		"graph.node.created.v1:identity:authActivity",
		"graph.node.created.v1:observability:invocation",
		"graph.node.created.v1:observability:codeMetric",
		"graph.node.deleted.v1:library:file",
	} {
		ex, ok := ExcludedFromForwarding(topic)
		if !ok {
			t.Errorf("%s is not covered by any recorded exclusion -- either the pattern stopped matching it, or the entry was removed without a replacement", topic)
			continue
		}
		if ForwardsGraphEvent(topic) {
			t.Errorf("%s is BOTH forwarded and recorded as excluded (%q).\n"+
				"One of the two is now wrong. If the forward is deliberate, delete the exclusion and say why in the rule's comment.",
				topic, ex.Reason)
		}
	}

	// The reachable positive: this instrument can move. v1:identity:user
	// CREATED is forwarded and must never be mistaken for its excluded
	// sibling -- per-user provisioning depends on it.
	if !ForwardsGraphEvent("graph.node.created.v1:identity:user") {
		t.Error("graph.node.created.v1:identity:user must stay forwarded -- per-user provisioning on signup is dead in cluster mode without it. Only the UPDATED verb is excluded.")
	}
}

// TestGraphEventTopicMatchesWhatTheEnginePublishes pins the one format string
// this package hands to callers against the shape component/memql actually
// publishes (concept_resolver.go builds "graph.node.%s.%s"). A gate asserting
// against a topic nothing publishes passes forever while measuring nothing.
func TestGraphEventTopicMatchesWhatTheEnginePublishes(t *testing.T) {
	if got, want := GraphEventTopic("created", "v1:worker:registration"), "graph.node.created.v1:worker:registration"; got != want {
		t.Fatalf("GraphEventTopic = %q, want %q", got, want)
	}
	// The wildcard rules are INTRA-segment globs: a concept id contains no
	// dots, so `v1:planner:*` is one segment matched by glob rather than a
	// segment wildcard. A re-implementation that assumed otherwise would
	// disagree with the real evaluator on exactly the rules that matter.
	if !ForwardsGraphEvent(GraphEventTopic("created", "v1:planner:task")) {
		t.Error("the v1:planner:* wildcard did not match v1:planner:task through the real evaluator")
	}
	if ForwardsGraphEvent(GraphEventTopic("created", "v1:nosuchdomain:thing")) {
		t.Error("default-deny broke: an unknown concept forwarded")
	}
}

// ---- harness -----------------------------------------------------------

type replica struct {
	bus    *events.Bus
	bridge *EventBridge
	out    chan *nodev1.NodeClientMessage
}

// twoReplicaMesh builds two replicas wired to each other by hand, and returns
// a deliver() that drains what A sent and hands each message to B's real
// inbound path. Cleanup is registered on t.
func twoReplicaMesh(t *testing.T) (a, b *replica, deliver func(*testing.T)) {
	t.Helper()

	mk := func(nodeId string) *replica {
		bus := events.NewBus(events.WithLogger(testLogger()))
		t.Cleanup(bus.Close)
		identity := testIdentity()
		identity.ID = nodeId
		pm := NewPeerManager(identity, testLogger())
		return &replica{
			bus:    bus,
			bridge: NewEventBridge(identity, bus, pm, testLogger()),
			out:    make(chan *nodev1.NodeClientMessage, 32),
		}
	}

	a, b = mk("bff-1"), mk("bff-2")

	// A knows about B and holds a connection to it. The connection's send
	// channel is what the gRPC stream would drain.
	a.bridge.peerManager.Register(&nodev1.PeerInfo{NodeId: "bff-2", NodeType: string(NodeTypeBFF)})
	a.bridge.peerManager.AttachConnection("bff-2", &peerConnection{
		nodeId: "bff-2",
		sendCh: a.out,
		logger: testLogger(),
	})

	// The bridge's own local-bus subscription is what onLocalEvent hangs off.
	// Start() would install it along with everything else a live node wires;
	// here only the subscription is wanted.
	unsub := a.bus.Subscribe("#", a.bridge.onLocalEvent, events.WithSubscriberName("test:bridge"))
	t.Cleanup(unsub)

	// The transport, as a goroutine rather than a drain call. A bus publish
	// reaches subscribers asynchronously, so a deliver() that drained
	// whatever happened to be queued at the moment it was called would race
	// the very hop it exists to observe -- and would fail INTERMITTENTLY,
	// which reads as a flaky mesh rather than as a broken harness. A pump
	// that runs for the test's lifetime is also what the gRPC stream it
	// stands in for actually does.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case msg := <-a.out:
				if fwd := msg.GetEventForward(); fwd != nil {
					b.bridge.HandleInbound(fwd)
				}
			case <-stop:
				return
			}
		}
	}()

	// Retained so a caller can make the hop explicit at the call site; the
	// pump above is what actually moves the message.
	deliver = func(t *testing.T) { t.Helper() }
	return a, b, deliver
}
