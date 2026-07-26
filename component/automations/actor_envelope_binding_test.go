package automations

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// memql#2801: an UNBOUND actor root is fail-open, so every evaluator
// that can reach an `actor.*` read must bind the denying envelope.
//
// The evaluator renders an unresolved dotted path as its own path TEXT,
// so with `actor` unbound `actor.isClusterOwner` evaluates to the
// non-empty string "actor.isClusterOwner" -- truthy. A negated gate
// (`actor.isClusterOwner != false`) therefore read TRUE on a request
// with no auth context, which is the same fail-open the envelope's nil
// default was fixed for.
//
// This is the coverage the first attempt at that fix shipped without:
// reverting the binding left the whole suite green.
func TestBindActorEnvelope_UnboundActorIsFailOpen(t *testing.T) {
	// Baseline: what an UNBOUND actor root does. This is not asserting
	// desired behaviour -- it documents why binding is mandatory.
	bare := NewEvaluator()
	got, err := bare.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("unbound evaluate: %v", err)
	}
	if !got {
		t.Skip("unbound path no longer renders as literal text; the fail-open premise changed -- revisit memql#2801")
	}

	// Bound: the denying envelope closes it.
	bound := NewEvaluator()
	bindActorEnvelope(context.Background(), bound)
	got, err = bound.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("bound evaluate: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner != false` is TRUE with no auth context -- the admin gate is fail-open (memql#2801)")
	}

	// The positive form must deny too.
	got, err = bound.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("bound evaluate ==: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner == true` must be false with no auth context")
	}
}

// A real owner must still pass, or the fix is a denial-of-service on the
// admin surface rather than a gate.
func TestBindActorEnvelope_RealOwnerStillPasses(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "u1", Role: auth.RoleOwner,
	})
	ev := NewEvaluator()
	bindActorEnvelope(ctx, ev)

	got, err := ev.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !got {
		t.Error("a real cluster owner must pass the gate")
	}
}

// The no-caller spelling (event triggers, scheduler ticks) must deny
// identically -- it is the same envelope, stated explicitly.
func TestBindNoCallerActorEnvelope_Denies(t *testing.T) {
	ev := NewEvaluator()
	bindNoCallerActorEnvelope(ev)
	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		got, err := ev.EvaluateCondition(cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got {
			t.Errorf("%s must be false for a trigger with no caller (memql#2801)", cond)
		}
	}
}

// End-to-end at the seam the whole memql#2801 narrative is built around:
// an event trigger's @filter. The structural invariant above proves the
// binding is PRESENT at every site; this proves it has the intended
// EFFECT on the path that matters most, because a `@filter` gating on an
// actor field decides whether the automation fires at all.
//
// `@filter(actor.isClusterOwner != false)` loads green (with or without
// @actor) and the compiler does not rewrite the actor root away, so an
// unbound root made this fire on every event.
func TestScheduler_EventFilterOnActor_DoesNotFireWithoutAuth(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	// Zero steps: a fire completes cleanly without a step registry, and the
	// assertion is on whether the filter admitted the event at all.
	gated := &Automation{
		Name:    "adminOnlySweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `actor.isClusterOwner != false`},
	}
	// Control: a filter that does NOT mention actor and is true. Without it
	// this test could pass because the harness never fires anything.
	control := &Automation{
		Name:    "ungatedSweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `event.topic != ""`},
	}
	for _, a := range []*Automation{gated, control} {
		s.automations[a.Name] = a
		if err := s.subscribeToEventTrigger(a); err != nil {
			t.Fatalf("subscribeToEventTrigger(%s): %v", a.Name, err)
		}
	}

	bus.PublishSync(events.NewEvent("node.created", events.KindNodeCreated, map[string]any{"id": "x"}))
	log := buf.String()

	// The bus carries no caller, so the denying envelope must make the
	// actor-gated filter false. Before the binding, the unbound
	// `actor.isClusterOwner` rendered as its own path text -- non-empty,
	// therefore truthy -- and this admitted every event.
	if !schedulerLogged(log, "filter not satisfied", gated.Name) {
		t.Errorf("the actor-gated @filter did NOT deny an event with no caller -- the actor root is "+
			"unbound or resolving truthy (memql#2801). Scheduler log:\n%s", log)
	}

	// The control asserts POSITIVELY that the harness fires. A negative
	// "the control was not denied" is satisfied by any early return that
	// skips the deny log -- a filter parse error, for instance -- which
	// would leave the assertion above proving nothing (review round 4).
	if !schedulerLogged(log, "event trigger fired", control.Name) {
		t.Errorf("the control (non-actor) automation did not fire, so the harness admits nothing and "+
			"the assertion above is vacuous. Scheduler log:\n%s", log)
	}
}

// schedulerLogged reports whether the scheduler logged the given message
// for the named automation.
//
// The name is matched on the structured `automation=` field rather than
// anywhere in the line: a bare substring match makes one automation's
// name match another's when it is a prefix (review round 4).
func schedulerLogged(log, msg, automation string) bool {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		if strings.Contains(line, "automation="+automation+" ") ||
			strings.HasSuffix(line, "automation="+automation) ||
			strings.Contains(line, `"automation":"`+automation+`"`) {
			return true
		}
	}
	return false
}
