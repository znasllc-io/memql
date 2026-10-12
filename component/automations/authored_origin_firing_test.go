package automations_test

// authored_origin_firing_test.go -- an authored EVENT automation fires on the
// node where its triggering event happened (memql#6022, F2).
//
// The owner gate used to cover event triggers as well as schedules: an owner's
// event rule ran only on the one node the gate assigned the owner to. Trigger
// events for arbitrary concepts are not forwarded between nodes (forwarding is
// default-deny), so a rule fired only when its triggering write happened to land
// on that node -- in the two-replica cluster, about half the writes sent
// nothing. These tests pin the split: an event trigger runs where its event
// happened and nowhere else, ungated; with no gate (a single node) both kinds
// of trigger run as they always did. That a schedule trigger still runs on the
// owner's node only is TestOwnerGate_SchedulerFiresOnlyOnOwningNode
// (authored_owner_gate_test.go).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

const originOwner = "v1:identity:user:origin-owner"

func newGatedScheduler(t *testing.T, bus *events.Bus, run automations.RunAutomationFunc, gate func(string) bool) *automations.AuthoredScheduler {
	t.Helper()
	s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:    automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()}),
		EventBus:  bus,
		Run:       run,
		OwnerGate: gate,
	})
	if err != nil {
		t.Fatalf("NewAuthoredScheduler: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

func userCreated(id string) events.Event {
	return events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": id})
}

const everySecondSrc = `
@trigger(schedule="* * * * * *")
automation everySecondSweep {
  run := logic sandboxNoopLogic(event: event)
}`

// TestAnEventRuleFiresOnTheNodeItsEventHappenedOn: this node does not own the
// rule's owner, and the triggering write happened here. The rule fires here,
// and the owner gate is not asked: nothing forwards the event to the owner's
// node, so a gate that said no here would mean the rule never fired at all.
func TestAnEventRuleFiresOnTheNodeItsEventHappenedOn(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	rec := &runRecorder{}
	var asked atomic.Int64
	s := newGatedScheduler(t, bus, rec.run, func(string) bool { asked.Add(1); return false })
	if err := s.Activate(authoredAutomationConstruct(originOwner, "onUserHere", eventAutomationSrc("onUserHere"))); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	bus.PublishSync(userCreated("v1:identity:user:written-here"))

	if rec.count() != 1 {
		t.Fatalf("a rule whose triggering write happened on this node fired %d times here, want 1: "+
			"no other node hears the event, so this is the only place it can fire", rec.count())
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the owner gate was consulted %d times for an event trigger; it decides where SCHEDULES run, not events", n)
	}
}

// TestAForwardedCopyOfAnEventFiresNothing: a concept the mesh broadcasts
// reaches every node, so each node sees the same write once as a local event
// (its origin) and the others as a forwarded copy. Only the origin fires. The
// node here owns the owner, which is exactly the node that used to fire the
// forwarded copy.
func TestAForwardedCopyOfAnEventFiresNothing(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	rec := &runRecorder{}
	s := newGatedScheduler(t, bus, rec.run, func(string) bool { return true })
	if err := s.Activate(authoredAutomationConstruct(originOwner, "onUserForwarded", eventAutomationSrc("onUserForwarded"))); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	forwarded := userCreated("v1:identity:user:written-elsewhere")
	forwarded.OriginNodeId = "node-a"
	bus.PublishSync(forwarded)
	if rec.count() != 0 {
		t.Fatalf("a forwarded copy fired the rule %d times; its origin fires it, so this is a second firing", rec.count())
	}

	// Control: the same rule fires for an event that happened here.
	bus.PublishSync(userCreated("v1:identity:user:written-here"))
	if rec.count() != 1 {
		t.Fatalf("control failed: a local event fired the rule %d times, want 1", rec.count())
	}
}

// TestOneWriteFiresAnEventRuleOnceAcrossTheMesh: two nodes, the owner gate
// assigned to node A, and a bridge carrying every local event to the other
// node as a forwarded copy, as the mesh does for a broadcast concept. A write
// on either node fires the rule once, on the node it happened on.
func TestOneWriteFiresAnEventRuleOnceAcrossTheMesh(t *testing.T) {
	loadConceptsForAuthored(t)
	busA, busB := events.NewBus(), events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })
	bridge := func(from string, to *events.Bus) func(events.Event) {
		return func(ev events.Event) {
			if ev.IsRemote() {
				return // never re-forward a copy, as EventBridge does not
			}
			ev.OriginNodeId = from
			to.PublishSync(ev)
		}
	}
	busA.Subscribe("graph.#", bridge("node-a", busB))
	busB.Subscribe("graph.#", bridge("node-b", busA))

	recA, recB := &runRecorder{}, &runRecorder{}
	a := newGatedScheduler(t, busA, recA.run, func(string) bool { return true })
	b := newGatedScheduler(t, busB, recB.run, func(string) bool { return false })
	for _, s := range []*automations.AuthoredScheduler{a, b} {
		if err := s.Activate(authoredAutomationConstruct(originOwner, "onUserAnywhere", eventAutomationSrc("onUserAnywhere"))); err != nil {
			t.Fatalf("Activate: %v", err)
		}
	}

	busB.PublishSync(userCreated("v1:identity:user:written-on-b"))
	if !eventuallyTrue(2*time.Second, func() bool { return recB.count() == 1 }) || recA.count() != 0 {
		t.Fatalf("a write on node B fired the rule %d times on B and %d on A, want once, on B", recB.count(), recA.count())
	}

	busA.PublishSync(userCreated("v1:identity:user:written-on-a"))
	if !eventuallyTrue(2*time.Second, func() bool { return recA.count() == 1 }) || recB.count() != 1 {
		t.Fatalf("a write on node A fired the rule %d times on A and %d more on B, want once, on A", recA.count(), recB.count()-1)
	}
}

// TestWithNoOwnerGateBothTriggersFireAsBefore: a single node has no gate, and
// both kinds of trigger fire there exactly as they did before the split.
func TestWithNoOwnerGateBothTriggersFireAsBefore(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	onEvent, onSchedule := &int32mu{}, &int32mu{}
	s := newGatedScheduler(t, bus, func(_ context.Context, a *automations.Automation, ev *events.Event) error {
		if ev == nil {
			onSchedule.inc()
		} else {
			onEvent.inc()
		}
		return nil
	}, nil)
	if err := s.Activate(authoredAutomationConstruct(originOwner, "onUserSingle", eventAutomationSrc("onUserSingle"))); err != nil {
		t.Fatalf("Activate event rule: %v", err)
	}
	if err := s.Activate(authoredAutomationConstruct(originOwner, "everySecondSweep", everySecondSrc)); err != nil {
		t.Fatalf("Activate schedule: %v", err)
	}

	bus.PublishSync(userCreated("v1:identity:user:single"))
	if onEvent.get() != 1 {
		t.Fatalf("with no gate a local event fired the rule %d times, want 1", onEvent.get())
	}
	if !eventuallyTrue(3*time.Second, func() bool { return onSchedule.get() >= 1 }) {
		t.Fatal("with no gate the schedule never ran")
	}
}

func eventuallyTrue(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
