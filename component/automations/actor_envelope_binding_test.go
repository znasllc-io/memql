package automations

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// evalActorCond parses a v1 condition and evaluates it over e.
func evalActorCond(t *testing.T, e *Evaluator, src string) bool {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	got, err := e.EvalV1Condition(context.Background(), n)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return got
}

// memql#2801: an actor gate must DENY when there is no actor.
//
// The string evaluator rendered an unresolved dotted path as its own path
// TEXT, so with `actor` unbound `actor.isClusterOwner != false` read TRUE;
// under the v1 absence table an ABSENT actor would read the same. Both are
// the fail-open the envelope's nil default was fixed for.
//
// An UNSEEDED RunScope therefore binds the DENYING actor -- the envelope a
// request with no auth context gets (auth.ActorEnvelopeMap(nil)) -- never an
// absent or empty one. No production path reaches an unseeded RunScope:
// every evaluator the runtime builds (the executor's run and onError
// evaluators, resume, the scheduler's trigger filter, the LogicRunner)
// binds the actor through bindActorEnvelope / bindNoCallerActorEnvelope,
// and Clone carries it. The guard is kept anyway, as the second line: an
// evaluator built by hand, or a new site that forgets the binder, denies
// rather than opening the admin gate.
func TestRunScope_UnseededActorDenies(t *testing.T) {
	bare := NewEvaluator()
	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		if evalActorCond(t, bare, cond) {
			t.Errorf("%s is TRUE over an unseeded run -- the admin gate is fail-open (memql#2801)", cond)
		}
	}

	// The event envelope carries an `actor` of its own (`{id}`, the
	// emitter's stamp, buildEventEnvelope). It is not the actor: reading it
	// as one leaves isClusterOwner absent, and `!= false` true.
	stamped := NewEvaluator()
	stamped.SetCustom("event", buildEventEnvelope(&events.Event{
		Topic: "node.created", Kind: events.KindNodeCreated,
		Payload:  map[string]any{"id": "x"},
		Metadata: map[string]string{"actor": "user-9"},
	}, "", ""))
	if evalActorCond(t, stamped, "actor.isClusterOwner != false") {
		t.Error("an unseeded run read the event envelope's `actor` stamp as the actor -- fail-open (memql#2801)")
	}

	// Seeded, the binder's envelope is what answers.
	bound := NewEvaluator()
	bindActorEnvelope(context.Background(), bound)
	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		if evalActorCond(t, bound, cond) {
			t.Errorf("%s must be false with no auth context", cond)
		}
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

	if !evalActorCond(t, ev, "actor.isClusterOwner == true") {
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
		if evalActorCond(t, ev, cond) {
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
// `@filter(row => actor.isClusterOwner != false)` loads green and the
// compiler does not rewrite the actor root away, so an unbound root made
// this fire on every event.
func TestScheduler_EventFilterOnActor_DoesNotFireWithoutAuth(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	// Zero steps: a fire completes cleanly without a step registry, and the
	// assertion is on whether the filter admitted the event at all.
	gated := &Automation{
		Name:    "adminOnlySweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `row => actor.isClusterOwner != false`},
	}
	// Control: a filter that does NOT mention actor and is true. Without it
	// this test could pass because the harness never fires anything.
	control := &Automation{
		Name:    "ungatedSweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `row => event.topic != ""`},
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
	// `actor.isClusterOwner` rendered as its own path text in the string
	// evaluator -- non-empty, therefore truthy -- and this admitted every
	// event.
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
