package clustere2e

// automation_run_routing_test.go is the cross-node gate for the automation
// invoke path (memql#3310).
//
// WHY THIS FILE CARRIES NO BUILD TAG
// ----------------------------------
// The rest of this package is `//go:build clustere2e` and needs a live
// 2-replica k3d cluster plus a seeded token. That is the right home for the
// end-to-end assertion (automation_run_test.go, next to this one), but it is
// SKIPPED everywhere a cluster is not running -- including on a developer's
// machine and on every CI lane that does not boot one. A gate that is skipped
// by default cannot be the thing standing between this feature and the mesh
// bug it was written to prevent.
//
// So the cross-node hop is ALSO gated here, deterministically and with no
// cluster, by wiring two in-process nodes through the SAME routing decision
// the real EventBridge applies (node.ForwardDecisionFor, which is
// evaluateRouting over defaultRoutingRules -- the exact call
// EventBridge.onLocalEvent makes) and the SAME structpb payload encoding the
// mesh forward does.
//
// HOW TO CONFIRM IT IS LOAD-BEARING
// ---------------------------------
// Delete component/node/routing_automation_run.go's init() (or its
// RegisterRoutingRule call) and run this file. The forward decision for
// automationrun.request goes false, node B never hears the request, and
// TestAutomationRunCrossesNodes fails with the relay's "no cognition node in
// the mesh picked up the run" refusal. Restore it and the test passes. If it
// passes both ways it is worthless, which is the whole risk this feature
// carries.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/node"
	"google.golang.org/protobuf/types/known/structpb"
)

// meshLink joins two in-process buses the way EventBridge joins two nodes:
// every locally-published event is offered to the routing rules, and only a
// forwardable one crosses -- carrying the origin node id that stops it from
// bouncing back, and round-tripping its payload through structpb exactly as
// the wire does.
type meshLink struct {
	t      *testing.T
	mu     sync.Mutex
	closed bool
	unsubs []func()

	// forwarded counts events that actually crossed, so a test can tell
	// "the rule dropped it" apart from "the relay never published".
	forwarded map[string]int
}

func newMeshLink(t *testing.T, aId string, a *events.Bus, bId string, b *events.Bus) *meshLink {
	l := &meshLink{t: t, forwarded: map[string]int{}}
	l.bridge(aId, a, b)
	l.bridge(bId, b, a)
	t.Cleanup(l.close)
	return l
}

func (l *meshLink) bridge(fromNodeId string, from, to *events.Bus) {
	unsub := from.Subscribe("#", func(evt events.Event) {
		// Never re-forward something that arrived from a peer -- the same
		// loop guard EventBridge.onLocalEvent applies via Event.IsRemote.
		if evt.OriginNodeId != "" {
			return
		}
		forward, _, _ := node.ForwardDecisionFor(evt.Topic)
		if !forward {
			return
		}

		// Fidelity that matters: the mesh encodes an event's payload with
		// structpb.NewStruct, which REJECTS any Go value it does not
		// recognise. A relay that put a raw automation step result on the
		// wire would fail here, silently, in production only. Encoding and
		// decoding for real means this test fails instead.
		encoded, err := structpb.NewStruct(evt.Payload)
		if err != nil {
			l.t.Errorf("event %q payload is not structpb-encodable, so it could never cross the mesh: %v",
				evt.Topic, err)
			return
		}

		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		l.forwarded[evt.Topic]++
		l.mu.Unlock()

		to.Publish(events.Event{
			Topic:        evt.Topic,
			Kind:         evt.Kind,
			Timestamp:    evt.Timestamp,
			Payload:      encoded.AsMap(),
			Metadata:     evt.Metadata,
			OriginNodeId: fromNodeId,
		})
	}, events.WithSubscriberName("test:meshLink:"+fromNodeId))

	l.mu.Lock()
	l.unsubs = append(l.unsubs, unsub)
	l.mu.Unlock()
}

func (l *meshLink) close() {
	l.mu.Lock()
	l.closed = true
	unsubs := l.unsubs
	l.unsubs = nil
	l.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
}

func (l *meshLink) count(topic string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forwarded[topic]
}

// runnerStub is a node's automation registry + dispatch. Only node B carries
// the automation, which is the point: the subscriber is compiled into a
// DIFFERENT node from the one the run is requested on.
type runnerStub struct {
	registry map[string]*automations.Automation
	steps    []*automations.StepResult

	mu   sync.Mutex
	runs int
}

func (r *runnerStub) LookupAutomation(name string) *automations.Automation { return r.registry[name] }

func (r *runnerStub) TriggerAutomationWithClientEvent(
	ctx context.Context, name string, event *events.Event,
) (*automations.AutomationExecution, error) {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()

	for _, s := range r.steps {
		automations.NotifyStepObserver(ctx, s)
	}
	exec := automations.NewExecution(name, "run:client")
	exec.Complete()
	return exec, nil
}

func (r *runnerStub) runCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

// collectSink is concurrency-safe: on a relayed run the frames are delivered
// from the bus's delivery goroutine, not from the caller's.
type collectSink struct {
	mu       sync.Mutex
	accepted []automations.RunAccepted
	steps    []automations.RunStep
	complete []automations.RunComplete
}

func (c *collectSink) Accepted(a automations.RunAccepted) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accepted = append(c.accepted, a)
}

func (c *collectSink) Step(s automations.RunStep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, s)
}

func (c *collectSink) Complete(x automations.RunComplete) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.complete = append(c.complete, x)
}

func (c *collectSink) snapshot() ([]automations.RunAccepted, []automations.RunStep, []automations.RunComplete) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]automations.RunAccepted(nil), c.accepted...),
		append([]automations.RunStep(nil), c.steps...),
		append([]automations.RunComplete(nil), c.complete...)
}

// twoNodeMesh stands up node A (bff, takes the request) and node B
// (cognition, owns the automation) joined by a routing-rule-respecting link.
type twoNodeMesh struct {
	a       *automations.RunRelay
	b       *automations.RunRelay
	bRunner *runnerStub
	link    *meshLink
}

func newTwoNodeMesh(t *testing.T, auto *automations.Automation, steps []*automations.StepResult) *twoNodeMesh {
	t.Helper()

	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(busA.Close)
	t.Cleanup(busB.Close)

	// Node A knows NOTHING about the automation. If the hop does not happen,
	// there is no way for the run to succeed by accident on the local path.
	runnerA := &runnerStub{registry: map[string]*automations.Automation{}}
	runnerB := &runnerStub{
		registry: map[string]*automations.Automation{auto.Name: auto},
		steps:    steps,
	}

	relayA, err := automations.NewRunRelay(automations.RunRelayOptions{
		Runner: runnerA, EventBus: busA, NodeId: "bff-1", NodeType: "bff",
	})
	if err != nil {
		t.Fatalf("relay A: %v", err)
	}
	relayB, err := automations.NewRunRelay(automations.RunRelayOptions{
		Runner: runnerB, EventBus: busB, NodeId: "cognition-1", NodeType: "cognition",
	})
	if err != nil {
		t.Fatalf("relay B: %v", err)
	}
	relayA.Start()
	relayB.Start()
	t.Cleanup(relayA.Stop)
	t.Cleanup(relayB.Stop)

	return &twoNodeMesh{
		a:       relayA,
		b:       relayB,
		bRunner: runnerB,
		link:    newMeshLink(t, "bff-1", busA, "cognition-1", busB),
	}
}

// THE GATE. The run is requested on the bff node; the automation exists only
// on the cognition node. It can only work if automationrun.request forwards
// across the mesh and automationrun.trace forwards back -- i.e. only if
// component/node/routing_automation_run.go's rule is registered.
func TestAutomationRunCrossesNodes(t *testing.T) {
	auto := &automations.Automation{
		Name:    "crossNodeSubject",
		Trigger: &automations.TriggerConfig{Event: "graph.node.created.v1:cognition:participant"},
	}
	mesh := newTwoNodeMesh(t, auto, []*automations.StepResult{
		{StepId: "loadRow", Status: "success", Duration: 2 * time.Millisecond},
		{StepId: "emitCard", Status: "success", Duration: 5 * time.Millisecond},
	})

	sink := &collectSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mesh.a.Run(ctx, automations.RunRequest{
		Automation:     auto.Name,
		Payload:        map[string]any{"id": "v1:cognition:participant:xyz"},
		TargetNodeType: "cognition",
	}, sink)

	accepted, steps, complete := sink.snapshot()

	if len(complete) != 1 {
		t.Fatalf("want exactly one complete frame, got %d", len(complete))
	}
	done := complete[0]

	if done.Status == "refused" {
		t.Fatalf("the cross-node run was refused: [%d] %s\n\n"+
			"This is what a MISSING ROUTING RULE looks like. Cross-node event delivery is "+
			"default-deny in component/node; without the automationrun.# forward rule "+
			"(component/node/routing_automation_run.go) the request never leaves the "+
			"requesting node and no peer ever sees it. Forward counts: request=%d trace=%d",
			done.ErrorCode, done.ErrorMessage,
			mesh.link.count(automations.TopicRunRequest),
			mesh.link.count(automations.TopicRunTrace))
	}
	if done.Status != "completed" || done.ErrorCode != automations.RunCodeOK {
		t.Fatalf("complete = %+v, want a completed run", done)
	}

	// The hop itself, asserted rather than assumed.
	if done.ExecutedOnNodeType != "cognition" || done.ExecutedOnNodeId != "cognition-1" {
		t.Fatalf("the automation must have run on the cognition node, got %s/%s",
			done.ExecutedOnNodeType, done.ExecutedOnNodeId)
	}
	if len(accepted) != 1 {
		t.Fatalf("want one accepted frame, got %d", len(accepted))
	}
	if accepted[0].RequestedOnNodeId != "bff-1" {
		t.Fatalf("the accepted frame must name the REQUESTING node, got %q", accepted[0].RequestedOnNodeId)
	}
	if accepted[0].RequestedOnNodeId == done.ExecutedOnNodeId {
		t.Fatal("requesting and executing node are the same -- this test did not exercise a cross-node hop")
	}
	if !accepted[0].RanDeployedDefinition || accepted[0].DefinitionNote == "" {
		t.Errorf("the banner the UI renders must survive the mesh hop, got %+v", accepted[0])
	}

	// The whole trace crossed, not just the terminator.
	if len(steps) != 2 {
		t.Fatalf("want 2 step frames back across the mesh, got %d", len(steps))
	}
	if steps[0].StepId != "loadRow" || steps[1].StepId != "emitCard" {
		t.Fatalf("step frames out of order or wrong: %+v", steps)
	}

	if mesh.bRunner.runCount() != 1 {
		t.Fatalf("the automation must have executed exactly once on node B, got %d", mesh.bRunner.runCount())
	}
	if got := mesh.link.count(automations.TopicRunRequest); got != 1 {
		t.Errorf("want exactly one request forward, got %d", got)
	}
	if got := mesh.link.count(automations.TopicRunTrace); got < 4 {
		t.Errorf("want the accepted + 2 steps + complete frames forwarded back, got %d", got)
	}
}

// The companion assertion: the routing decision this harness depends on is the
// real one. If the rule is ever dropped, this fails immediately and names the
// file, so the 15-second timeout above is not the only diagnosis on offer.
func TestAutomationRunRoutingRuleIsRegistered(t *testing.T) {
	for _, topic := range []string{automations.TopicRunRequest, automations.TopicRunTrace} {
		forward, broadcast, _ := node.ForwardDecisionFor(topic)
		if !forward || !broadcast {
			t.Fatalf("topic %q must broadcast across the mesh (forward=%v broadcast=%v) -- "+
				"see component/node/routing_automation_run.go; without it the automation "+
				"invoke path works on one node and dies silently in the mesh",
				topic, forward, broadcast)
		}
	}
	// And the adjacent tree it must NOT be confused with.
	if forward, _, _ := node.ForwardDecisionFor("automation.completed"); forward {
		t.Fatal("automation.# lifecycle chatter must stay node-local")
	}
	if !strings.HasPrefix(automations.TopicRunRequest, "automationrun.") {
		t.Fatalf("the run topics must stay OUT of the blocked automation.# tree, got %q",
			automations.TopicRunRequest)
	}
}
