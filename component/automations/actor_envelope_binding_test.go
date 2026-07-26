package automations

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// The structural invariant, and the reason it exists: three review rounds
// on memql#2801 each found ANOTHER evaluator that left `actor` unbound,
// and a helper-level test catches none of them -- removing a call site
// leaves the suite green.
//
// So rather than one behavioural test per seam and a hole the next time
// somebody adds a sixth evaluator, this asserts the invariant directly:
// every NewEvaluator() in this package must bind an actor envelope.
//
// An unbound actor root is not neutral. The evaluator renders an
// unresolved dotted path as its own path TEXT, so `actor.isClusterOwner`
// is a non-empty (truthy) string and a negated gate reads TRUE with no
// auth context.
func TestEveryEvaluatorBindsAnActorEnvelope(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	type site struct {
		file string
		fn   string
		line int
	}
	var unbound []site
	checked := 0

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "actor_envelope_binding.go" {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		lines := strings.Split(string(src), "\n")

		// Walk function-by-function: a construction and its binding can sit
		// many lines apart (newEvaluatorForLogic seeds a dozen roots first),
		// so a fixed line window both misses real pairs and invents false
		// ones. The enclosing function is the honest unit.
		fnStart, fnName := -1, ""
		flush := func(end int) {
			if fnStart < 0 {
				return
			}
			body := strings.Join(lines[fnStart:end], "\n")
			// The declaration of NewEvaluator itself is not a call site.
			if fnName == "NewEvaluator" || !strings.Contains(body, "NewEvaluator()") {
				fnStart = -1
				return
			}
			checked++
			if !strings.Contains(body, "bindActorEnvelope(") &&
				!strings.Contains(body, "bindNoCallerActorEnvelope(") {
				unbound = append(unbound, site{f, fnName, fnStart + 1})
			}
			fnStart = -1
		}
		for i, line := range lines {
			if strings.HasPrefix(line, "func ") {
				flush(i)
				fnStart = i
				fnName = funcNameFrom(line)
			}
		}
		flush(len(lines))
	}

	if checked == 0 {
		t.Fatal("no NewEvaluator() call sites found; the scan must not pass vacuously")
	}
	for _, s := range unbound {
		t.Errorf("%s:%d (%s) constructs an evaluator without binding an actor envelope -- "+
			"an unbound actor.* read renders as its own path text and is TRUTHY, so a "+
			"negated actor gate fails OPEN (memql#2801). Call bindActorEnvelope(ctx, ev), "+
			"or bindNoCallerActorEnvelope(ev) where there is provably no caller.", s.file, s.line, s.fn)
	}
	t.Logf("checked %d function(s) constructing an evaluator", checked)
}

// funcNameFrom pulls the identifier out of a `func` declaration line,
// handling both plain and method receivers.
func funcNameFrom(line string) string {
	rest := strings.TrimPrefix(line, "func ")
	if strings.HasPrefix(rest, "(") {
		if i := strings.Index(rest, ")"); i >= 0 {
			rest = strings.TrimSpace(rest[i+1:])
		}
	}
	if i := strings.IndexAny(rest, "("); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
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
	if !filterDeniedFor(log, gated.Name) {
		t.Errorf("the actor-gated @filter did NOT deny an event with no caller -- the actor root is "+
			"unbound or resolving truthy (memql#2801). Scheduler log:\n%s", log)
	}
	// Control: the harness really does admit events, so the assertion above
	// is meaningful rather than vacuous.
	if filterDeniedFor(log, control.Name) {
		t.Errorf("the control (non-actor) filter was denied too -- the harness is rejecting everything, "+
			"so the assertion above proves nothing. Scheduler log:\n%s", log)
	}
}

// filterDeniedFor reports whether the scheduler logged a filter miss for
// the named automation.
func filterDeniedFor(log, automation string) bool {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "filter not satisfied") && strings.Contains(line, automation) {
			return true
		}
	}
	return false
}
